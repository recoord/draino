/*
Copyright 2018 Planet Labs Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
implied. See the License for the specific language governing permissions
and limitations under the License.
*/

package kubernetes

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	url2 "net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/planetlabs/draino/internal/kubernetes/k8sclient"

	"github.com/DataDog/go-service-authn/pkg/serviceauthentication/authnclient"
	"go.opencensus.io/tag"
	"go.uber.org/zap"
	httptrace "gopkg.in/DataDog/dd-trace-go.v1/contrib/net/http"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"
	core "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	policy "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	runtimejson "k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

// Default pod eviction settings.
const (
	SetConditionTimeout     = 10 * time.Second
	SetConditionRetryPeriod = 50 * time.Millisecond

	CustomRetryMaxAttemptAnnotation = "draino/retry-max-attempt"

	DefaultMinEvictionTimeout           = 8 * time.Minute
	DefaultEvictionOverhead             = 30 * time.Second
	DefaultPVCRecreateTimeout           = 3 * time.Minute
	DefaultPodDeletePeriodWaitingForPVC = 10 * time.Second
	awaitPVCDeletionTimeout             = time.Minute

	KindDaemonSet   = "DaemonSet"
	KindStatefulSet = "StatefulSet"

	ConditionDrainedScheduled = "DrainScheduled"
	DefaultSkipDrain          = false

	EvictionNodeConditionsAnnotationKey        = "draino/node-conditions"
	PVCStorageClassCleanupAnnotationKey        = "draino/delete-pvc-and-pv"
	PVCStorageClassCleanupAnnotationTrueValue  = "true"
	PVCStorageClassCleanupAnnotationFalseValue = "false"

	CompletedStr = "Completed"
	FailedStr    = "Failed"
	ScheduledStr = "Scheduled"

	NodeLabelKeyReplaceRequest     = "node.datadoghq.com/replace"
	NodeLabelValueReplaceRequested = "requested"
	NodeLabelValueReplaceDone      = "done"
	NodeLabelValueReplaceFailed    = "failed"

	eventReasonEvictionStarting      = "EvictionStarting"
	eventReasonEvictionSucceeded     = "EvictionSucceeded"
	eventReasonEvictionFailed        = "EvictionFailed"
	eventReasonEvictionAttemptFailed = "EvictionAttemptFailed"

	eventReasonBadValueForAnnotation = "BadValueForAnnotation"

	EvictionAPIURLAnnotationKey = "draino/eviction-api-url"
)

type nodeMutatorFn func(*core.Node)

type EvictionEndpointError struct {
	StatusCode          int
	AfterSeveralRetries bool
	IsRequestTimeout    bool
}

func (e EvictionEndpointError) Error() string {
	msg := "eviction endpoint error"
	if e.IsRequestTimeout {
		msg += ": timeout while waiting for eviction endpoint response."
	}
	if e.StatusCode > 0 {
		msg += fmt.Sprintf(": code=%d", e.StatusCode)
	}
	if e.AfterSeveralRetries {
		msg += " after several retries"
	}
	return msg
}

type AudienceNotFoundError struct {
	Audience string
}

func (e AudienceNotFoundError) Error() string {
	return fmt.Sprintf("Emissary requested a token for the %s audience from Vault, but Vault couldn't find the audience. Audiences must be created in [vault-config](https://github.com/DataDog/vault-config/blob/main/terraform/oidc-token-provider.tf#L2-L4) for now.", e.Audience)
}

type NodePreprovisioningTimeoutError struct {
}

func (e NodePreprovisioningTimeoutError) Error() string {
	return "timed out waiting for node pre-provisioning"
}

type NodeHasNotDrainingTaintError struct {
	NodeName string
}

func (e NodeHasNotDrainingTaintError) Error() string {
	return "the node " + e.NodeName + " is not tainted 'draining'"
}

type PodEvictionTimeoutError struct {
	isEvictionPP bool
}

func (e PodEvictionTimeoutError) Error() string {
	msg := "timed out waiting for eviction;"
	if e.isEvictionPP {
		msg += " eviction++ endpoint was not able to finish request in time."
	} else {
		msg += " most likely related to missing disruption budget."
	}
	return msg
}

type OverlappingDisruptionBudgetsError struct {
}

func (e OverlappingDisruptionBudgetsError) Error() string {
	return "overlapping pod disruption budgets"
}

type PodDeletionTimeoutError struct {
}

func (e PodDeletionTimeoutError) Error() string {
	return "timed out waiting for pod to be deleted (stuck terminating, check finalizers)"
}

type VolumeCleanupError struct {
	Err error
}

func (e VolumeCleanupError) Error() string {
	return "error while cleaning up volumes: " + e.Err.Error()
}

func (e VolumeCleanupError) Unwrap() error {
	return e.Err
}

// A Drainer drains nodes.
type Drainer interface {
	// Drain the supplied node. Evicts the node of all but mirror and DaemonSet pods.
	Drain(ctx context.Context, n *core.Node) error
	MarkDrain(ctx context.Context, n *core.Node, when, finish time.Time, failed bool, failCount int32) error
	MarkDrainDelete(ctx context.Context, n *core.Node) error
	GetPodsToDrain(ctx context.Context, node string, podStore PodStore) ([]*core.Pod, error)
	GetMaxDrainAttemptsBeforeFail(ctx context.Context, n *core.Node) int32
	ResetRetryAnnotation(ctx context.Context, n *core.Node) error
}

type NodeReplacementStatus string

const (
	NodeReplacementStatusRequested NodeReplacementStatus = "requested"
	NodeReplacementStatusDone      NodeReplacementStatus = "done"
	NodeReplacementStatusFailed    NodeReplacementStatus = "failed"
)

// A NodeReplacer helps to request for node replacement
type NodeReplacer interface {
	ReplaceNode(ctx context.Context, n *core.Node) (bool, error)
	PreprovisionNode(ctx context.Context, n *core.Node) error
	GetReplacementStatus(ctx context.Context, n *core.Node) (NodeReplacementStatus, error)
}

// DrainerInstance is able to Drain or Replace Node
type DrainerInstance interface {
	Drainer
	NodeReplacer
}

type DrainerNodeReplacer interface {
	Drainer
	NodeReplacer
}

// A NoopDrainer does nothing.
type NoopDrainer struct{}

func (d *NoopDrainer) GetPodsToDrain(ctx context.Context, node string, podStore PodStore) ([]*core.Pod, error) {
	return nil, nil
}

// Drain does nothing.
func (d *NoopDrainer) Drain(ctx context.Context, n *core.Node) error { return nil }

// ResetRetryAnnotation does nothing.
func (d *NoopDrainer) ResetRetryAnnotation(ctx context.Context, n *core.Node) error { return nil }

// MarkDrain does nothing.
func (d *NoopDrainer) MarkDrain(ctx context.Context, n *core.Node, when, finish time.Time, failed bool, failCount int32) error {
	return nil
}

// MarkDrainDelete does nothing.
func (d *NoopDrainer) MarkDrainDelete(ctx context.Context, n *core.Node) error {
	return nil
}

func (d *NoopDrainer) GetMaxDrainAttemptsBeforeFail(ctx context.Context, n *core.Node) int32 {
	return 0
}

// ReplaceNode return none
func (d *NoopDrainer) ReplaceNode(ctx context.Context, n *core.Node) (bool, error) {
	return false, nil
}

// PreprovisionNode return none
func (d *NoopDrainer) PreprovisionNode(ctx context.Context, n *core.Node) error {
	return nil
}

// PreprovisionNode return none
func (d *NoopDrainer) GetReplacementStatus(ctx context.Context, n *core.Node) (NodeReplacementStatus, error) {
	return "", nil
}

var _ DrainerInstance = &APIDrainer{}

// APIDrainer drains Kubernetes nodes via the Kubernetes API.
type APIDrainer struct {
	crClient           client.Client
	c                  kubernetes.Interface
	l                  *zap.Logger
	eventRecorder      EventRecorder
	runtimeObjectStore RuntimeObjectStore

	filter PodFilterFunc

	minEvictionTimeout         time.Duration
	evictionHeadroom           time.Duration
	skipDrain                  bool
	maxDrainAttemptsBeforeFail int32

	globalConfig GlobalConfig

	storageClassesAllowingPVDeletion map[string]struct{}
}

// APIDrainerOption configures an APIDrainer.
type APIDrainerOption func(d *APIDrainer)

// MaxGracePeriod configures the maximum time to wait for a pod eviction. Pod
// containers will be allowed this much time to shutdown once they receive a
// SIGTERM before they are sent a SIGKILL.
func MaxGracePeriod(m time.Duration) APIDrainerOption {
	return func(d *APIDrainer) {
		d.minEvictionTimeout = m
	}
}

// EvictionHeadroom configures an amount of time to wait in addition to the
// MaxGracePeriod for the API server to report a pod deleted.
func EvictionHeadroom(h time.Duration) APIDrainerOption {
	return func(d *APIDrainer) {
		d.evictionHeadroom = h
	}
}

// WithPodFilter configures a filter that may be used to exclude certain pods
// from eviction when draining.
func WithPodFilter(f PodFilterFunc) APIDrainerOption {
	return func(d *APIDrainer) {
		d.filter = f
	}
}

// WithDrain determines if we're actually going to drain nodes
func WithSkipDrain(b bool) APIDrainerOption {
	return func(d *APIDrainer) {
		d.skipDrain = b
	}
}

// WithAPIDrainerLogger configures a APIDrainer to use the supplied
// logger.
func WithAPIDrainerLogger(l *zap.Logger) APIDrainerOption {
	return func(d *APIDrainer) {
		d.l = l
	}
}

// WithStorageClassesAllowingDeletion configures an APIDrainer to allow deletion of PV/PVC for some storage classes only
func WithStorageClassesAllowingDeletion(storageClasses []string) APIDrainerOption {
	return func(d *APIDrainer) {
		d.storageClassesAllowingPVDeletion = map[string]struct{}{}
		for _, sc := range storageClasses {
			d.storageClassesAllowingPVDeletion[sc] = struct{}{}
		}
	}
}

// WithMaxDrainAttemptsBeforeFail configures the max count of failed drain attempts before a final fail
func WithMaxDrainAttemptsBeforeFail(maxDrainAttemptsBeforeFail int) APIDrainerOption {
	return func(d *APIDrainer) {
		d.maxDrainAttemptsBeforeFail = int32(maxDrainAttemptsBeforeFail)
	}
}

// WithRuntimeObjectStore configures the runtime object store
func WithRuntimeObjectStore(store RuntimeObjectStore) APIDrainerOption {
	return func(d *APIDrainer) {
		d.runtimeObjectStore = store
	}
}

// WithGlobalConfig give the list of conditions for which draino is triggered
func WithGlobalConfig(globalConfig GlobalConfig) APIDrainerOption {
	return func(d *APIDrainer) {
		d.globalConfig = globalConfig
	}
}

func WithContainerRuntimeClient(client client.Client) APIDrainerOption {
	return func(d *APIDrainer) {
		d.crClient = client
	}
}

// NewAPIDrainer returns a Drainer that drains nodes
func NewAPIDrainer(c kubernetes.Interface, eventRecorder EventRecorder, ao ...APIDrainerOption) *APIDrainer {
	d := &APIDrainer{
		c:                  c,
		l:                  zap.NewNop(),
		filter:             NewPodFilters(),
		minEvictionTimeout: DefaultMinEvictionTimeout,
		evictionHeadroom:   DefaultEvictionOverhead,
		skipDrain:          DefaultSkipDrain,
		eventRecorder:      eventRecorder,
	}
	for _, o := range ao {
		o(d)
	}
	return d
}

func GetNodeRetryMaxAttempt(n *core.Node) (customValue int32, usedDefault bool, err error) {
	if maxStr, ok := n.Annotations[CustomRetryMaxAttemptAnnotation]; ok {
		maxValue, err := strconv.Atoi(maxStr)
		if err != nil {
			return 0, true, fmt.Errorf(CustomRetryMaxAttemptAnnotation+" can't convert value. Ignoring the user value '%s' and using default instead. Error: %w", maxStr, err)
		}
		if maxValue < 1 { // to disable retry the user should use annotation draino/drain-retry=false
			return 0, true, fmt.Errorf(CustomRetryMaxAttemptAnnotation+" has a zero or negative value. Ignoring the value '%s' and using default instead.", maxStr)
		}
		if maxValue > 100 { // it does not make sense to have bigger value. User should play with `retry-delay` parameter at some point to increase the retry period
			return 100, false, fmt.Errorf(CustomRetryMaxAttemptAnnotation+" has a too big value '%s'. Ignoring the value and using 100 instead.", maxStr)
		}
		return int32(maxValue), false, nil
	}
	return 0, true, nil
}

func (d *APIDrainer) GetMaxDrainAttemptsBeforeFail(ctx context.Context, n *core.Node) int32 {
	customValue, useDefault, err := GetNodeRetryMaxAttempt(n)
	if err != nil {
		d.l.Warn(err.Error(), zap.String("node", n.Name))
		d.eventRecorder.NodeEventf(ctx, n, core.EventTypeWarning, eventReasonBadValueForAnnotation, err.Error())
	}
	if useDefault {
		return d.maxDrainAttemptsBeforeFail
	}
	return customValue
}

func (d *APIDrainer) ResetRetryAnnotation(ctx context.Context, n *core.Node) error {
	span, ctx := tracer.StartSpanFromContext(ctx, "ResetRetryAnnotation")
	defer span.Finish()

	if _, ok := n.Labels[NodeLabelKeyReplaceRequest]; ok {
		k8sclient.PatchDeleteNodeLabelKey(ctx, d.c, n.Name, NodeLabelKeyReplaceRequest)
	}

	// Till we are done with the annotation migration to the new key we have to deal with the 2 keys. Later we can remove that first block.
	if n.Annotations[drainRetryAnnotationKey] == drainRetryFailedAnnotationValue {
		k8sclient.PatchNodeAnnotationKey(ctx, d.c, n.Name, drainRetryAnnotationKey, drainRetryAnnotationValue)
	}
	return k8sclient.PatchDeleteNodeAnnotationKey(ctx, d.c, n.Name, drainRetryFailedAnnotationKey)
}

// MarkDrainDelete removes the condition on the node to mark the current drain schedule.
func (d *APIDrainer) MarkDrainDelete(ctx context.Context, n *core.Node) error {
	span, ctx := tracer.StartSpanFromContext(ctx, "MarkDrainDelete")
	defer span.Finish()

	if err := RetryWithTimeout(
		func() error {
			nodeName := n.Name
			// Refresh the node object
			freshNode, err := d.c.CoreV1().Nodes().Get(ctx, nodeName, meta.GetOptions{})
			if err != nil {
				if !apierrors.IsNotFound(err) {
					return err
				}
				return nil
			}
			newConditions := []core.NodeCondition{}
			for _, condition := range freshNode.Status.Conditions {
				if string(condition.Type) == ConditionDrainedScheduled {
					continue
				}
				newConditions = append(newConditions, condition)
			}
			if len(newConditions) == len(freshNode.Status.Conditions) {
				return nil
			}
			freshNode.Status.Conditions = newConditions
			if _, err := d.c.CoreV1().Nodes().UpdateStatus(ctx, freshNode, meta.UpdateOptions{FieldManager: "draino"}); err != nil {
				return err
			}
			return nil
		},
		SetConditionRetryPeriod,
		SetConditionTimeout,
	); err != nil {
		return err
	}
	return nil
}

// MarkDrain set a condition on the node to mark that the drain is scheduled. (retry internally in case of failure)
func (d *APIDrainer) MarkDrain(ctx context.Context, n *core.Node, when, finish time.Time, failed bool, failCount int32) error {
	span, ctx := tracer.StartSpanFromContext(ctx, "MarkDrain")
	defer span.Finish()

	span.SetTag("node", n.GetName())
	span.SetTag("when", when)
	span.SetTag("finish", finish)
	span.SetTag("failCount", failCount)
	span.SetTag("failed", failed)

	if err := RetryWithTimeout(
		func() error {
			nodeName := n.Name
			// Refresh the node object
			freshNode, err := d.c.CoreV1().Nodes().Get(ctx, nodeName, meta.GetOptions{})
			if err != nil {
				if !apierrors.IsNotFound(err) {
					return err
				}
				return nil
			}

			msgSuffix := ""
			conditionStatus := core.ConditionTrue
			if !finish.IsZero() {
				if failed {
					msgSuffix = fmt.Sprintf(" | %s: %s", FailedStr, finish.Format(time.RFC3339))
					if failCount >= d.GetMaxDrainAttemptsBeforeFail(ctx, n) {
						freshNode.Annotations[drainRetryFailedAnnotationKey] = drainRetryFailedAnnotationValue
					}
				} else {
					msgSuffix = fmt.Sprintf(" | %s: %s", CompletedStr, finish.Format(time.RFC3339))
				}
				conditionStatus = core.ConditionFalse
			}

			// Create or update the condition associated to the monitor
			now := meta.Time{Time: time.Now()}
			conditionUpdated := false
			msgPrefix := fmt.Sprintf("[%d] | ", failCount)
			for i, condition := range freshNode.Status.Conditions {
				if string(condition.Type) != ConditionDrainedScheduled {
					continue
				}
				freshNode.Status.Conditions[i].LastHeartbeatTime = now
				freshNode.Status.Conditions[i].Message = msgPrefix + "Drain activity scheduled " + when.Format(time.RFC3339) + msgSuffix
				freshNode.Status.Conditions[i].Status = conditionStatus
				conditionUpdated = true
			}
			if !conditionUpdated { // There was no condition found, let's create one
				freshNode.Status.Conditions = append(freshNode.Status.Conditions,
					core.NodeCondition{
						Type:               ConditionDrainedScheduled,
						Status:             conditionStatus,
						LastHeartbeatTime:  now,
						LastTransitionTime: now,
						Reason:             "Draino",
						Message:            msgPrefix + "Drain activity scheduled " + when.Format(time.RFC3339) + msgSuffix,
					},
				)
			}
			if _, err := d.c.CoreV1().Nodes().UpdateStatus(ctx, freshNode, meta.UpdateOptions{FieldManager: "draino"}); err != nil {
				return err
			}
			return nil
		},
		SetConditionRetryPeriod,
		SetConditionTimeout,
	); err != nil {
		return err
	}
	return nil
}

type DrainConditionStatus struct {
	Marked         bool
	Completed      bool
	Failed         bool
	FailedCount    int32
	LastTransition time.Time
}

func GetDrainConditionStatus(n *core.Node) (DrainConditionStatus, error) {
	var drainStatus DrainConditionStatus
	for _, condition := range n.Status.Conditions {
		if string(condition.Type) != ConditionDrainedScheduled {
			continue
		}
		drainStatus.Marked = true
		drainStatus.LastTransition = condition.LastTransitionTime.Time
		drainStatus.FailedCount = 0
		msg := strings.Split(condition.Message, " | ")
		msgPrefix := msg[0]
		if len(msgPrefix) > 0 && msgPrefix[0] == '[' { // Detect new prefix format
			_, err := fmt.Sscanf(msgPrefix, "[%d]", &drainStatus.FailedCount)
			if err != nil {
				return drainStatus, fmt.Errorf("cannot parse failedCount on node%s: %w", n.GetName(), err)
			}
		}

		if condition.Status == core.ConditionFalse {
			if strings.Contains(condition.Message, CompletedStr) {
				drainStatus.Completed = true
				return drainStatus, nil
			}
			if strings.Contains(condition.Message, FailedStr) {
				// [1] | Drain activity scheduled 2020-03-20T15:50:34+01:00 | Failed: 2020-03-20T15:55:50+01:00
				drainStatus.Failed = true
				return drainStatus, nil
			}
		} else if condition.Status == core.ConditionTrue {
			return drainStatus, nil
		}
		return drainStatus, errors.New("failed to read " + ConditionDrainedScheduled + " condition")
	}
	return drainStatus, nil
}

// Drain the supplied node. Evicts the node of all but mirror and DaemonSet pods.
func (d *APIDrainer) Drain(ctx context.Context, node *core.Node) error {
	span, ctx := tracer.StartSpanFromContext(ctx, "Drain")
	defer span.Finish()

	// Do nothing if draining is not enabled.
	if d.skipDrain {
		TracedLoggerForNode(ctx, node, d.l).Debug("Skipping drain because draining is disabled")
		return nil
	}

	// retrieve a fresh version of the node
	n, err := d.c.CoreV1().Nodes().Get(ctx, node.Name, meta.GetOptions{})
	if err != nil {
		return err
	}

	taint, hasNLATaint := k8sclient.GetNLATaint(n)
	drainCandidate := hasNLATaint && taint.Value == k8sclient.TaintDraining

	if !drainCandidate {
		TracedLoggerForNode(ctx, node, d.l).Info("Aborting drain because the node is not drain-candidate")
		return NodeHasNotDrainingTaintError{NodeName: node.Name}
	}

	pods, err := d.GetPodsToDrain(ctx, n.GetName(), nil)
	if err != nil {
		return fmt.Errorf("cannot get pods for node %s: %w", n.GetName(), err)
	}

	abort := make(chan struct{})
	errs := make(chan error, 1)
	for i := range pods {
		pod := pods[i]
		go func() {
			d.eventRecorder.NodeEventf(ctx, n, core.EventTypeNormal, eventReasonEvictionStarting, "Evicting pod %s/%s to drain node", pod.Namespace, pod.Name)
			d.eventRecorder.PodEventf(ctx, pod, core.EventTypeNormal, eventReasonEvictionStarting, "Evicting pod to drain node %s", n.Name)
			if err := d.evict(ctx, n, pod, abort); err != nil {
				d.eventRecorder.NodeEventf(ctx, n, core.EventTypeWarning, eventReasonEvictionFailed, "Eviction failed for pod %s/%s: %v", pod.Namespace, pod.Name, err)
				d.eventRecorder.PodEventf(ctx, pod, core.EventTypeWarning, eventReasonEvictionFailed, "Eviction failed: %v", err)
				errs <- fmt.Errorf("cannot evict pod %s/%s: %w", pod.GetNamespace(), pod.GetName(), err)
				return
			}
			d.eventRecorder.NodeEventf(ctx, n, core.EventTypeNormal, eventReasonEvictionSucceeded, "Pod %s/%s evicted from node", pod.Namespace, pod.Name)
			d.eventRecorder.PodEventf(ctx, pod, core.EventTypeNormal, eventReasonEvictionSucceeded, "Pod evicted from node %s", n.Name)
			errs <- nil // the for range pods below expects to receive one value per pod from the errs channel
		}()
	}
	// This will _eventually_ abort evictions. Evictions may spend up to
	// d.deleteTimeout() in d.awaitDeletion(), or 5 seconds in backoff before
	// noticing they've been aborted.
	//
	// Note(adrienjt): In addition, they may also spend up to:
	// - 1min/PVC awaiting PVC deletions,
	// - 1min/PV awaiting PV deletions,
	// - and DefaultPVCRecreateTimeout per PVC
	defer close(abort)

	for range pods {
		if err := <-errs; err != nil {
			return fmt.Errorf("cannot evict all pods: %w", err)
			// all remaining evictions are aborted and their errors ignored (aborted or otherwise)
			// TODO(adrienjt): capture missing errors?
			// They are registered as events on pods.
		}
	}
	return nil
}

func (d *APIDrainer) GetPodsToDrain(ctx context.Context, node string, podStore PodStore) ([]*core.Pod, error) {
	span, ctx := tracer.StartSpanFromContext(ctx, "GetPodsToDrain")
	defer span.Finish()

	var err error
	var pods []*core.Pod
	if podStore != nil {
		if pods, err = podStore.ListPodsForNode(node); err != nil {
			return nil, err
		}
	} else {
		l, err := d.c.CoreV1().Pods(meta.NamespaceAll).List(ctx, meta.ListOptions{
			FieldSelector: fields.SelectorFromSet(fields.Set{"spec.nodeName": node}).String(),
		})
		if err != nil {
			return nil, fmt.Errorf("cannot get pods for node %s: %w", node, err)
		}
		for i := range l.Items {
			pods = append(pods, &l.Items[i])
		}
	}

	include := make([]*core.Pod, 0, len(pods))
	for _, p := range pods {
		passes, _, err := d.filter(*p)
		if err != nil {
			return nil, fmt.Errorf("cannot filter pods: %w", err)
		}
		if passes {
			include = append(include, p)
		}
	}
	return include, nil
}

func (d *APIDrainer) evict(ctx context.Context, node *core.Node, pod *core.Pod, abort <-chan struct{}) error {
	evictionAPIURL, ok := GetAnnotationFromPodOrController(EvictionAPIURLAnnotationKey, pod, d.runtimeObjectStore)
	if ok {
		return d.evictWithOperatorAPI(ctx, evictionAPIURL, node, pod, abort)
	}
	return d.evictWithKubernetesAPI(ctx, node, pod, abort)
}

func (d *APIDrainer) getGracePeriodWithEvictionHeadRoom(pod *core.Pod) time.Duration {
	gracePeriod := int64(core.DefaultTerminationGracePeriodSeconds)
	if pod.Spec.TerminationGracePeriodSeconds != nil {
		gracePeriod = *pod.Spec.TerminationGracePeriodSeconds
	}
	return time.Duration(gracePeriod)*time.Second + d.evictionHeadroom
}

func (d *APIDrainer) getMinEvictionTimeoutWithEvictionHeadRoom(pod *core.Pod) time.Duration {
	gracePeriod := d.minEvictionTimeout
	if pod.Spec.TerminationGracePeriodSeconds != nil && time.Duration(*pod.Spec.TerminationGracePeriodSeconds)*time.Second > gracePeriod {
		gracePeriod = time.Duration(*pod.Spec.TerminationGracePeriodSeconds) * time.Second
	}
	return gracePeriod + d.evictionHeadroom
}

func (d *APIDrainer) evictWithKubernetesAPI(ctx context.Context, node *core.Node, pod *core.Pod, abort <-chan struct{}) error {
	span, ctx := tracer.StartSpanFromContext(ctx, "evictWithKubernetesAPI")
	defer span.Finish()

	return d.evictionSequence(ctx, node, pod, abort,
		// eviction function
		func() error {
			return d.c.CoreV1().Pods(pod.GetNamespace()).EvictV1(ctx, &policy.Eviction{
				ObjectMeta: meta.ObjectMeta{Namespace: pod.GetNamespace(), Name: pod.GetName()},
			})
		},
		// error handling function
		func(err error) error {
			// The eviction API returns 500 if a pod
			// matches more than one pod disruption budgets.
			// We cannot use apierrors.IsInternalError because Reason is not set, just Code and Message.
			if statErr, ok := err.(apierrors.APIStatus); ok && statErr.Status().Code == 500 {
				return OverlappingDisruptionBudgetsError{} // this one is typed because we match it to a failure cause
			}
			return err // unexpected (we're already catching 429 and 500), may be a client side error
		},
	)

}

// evictWithOperatorAPI This function calls an Operator endpoint to perform the eviction instead of the classic kubernetes eviction endpoint
// The endpoint should support the same payload than the kubernetes eviction endpoint.
// The expected responses are:
// 200/201: eviction accepted and performed/performing
// 429    : need to retry later, equivalent to no credit in PDB
// 404    : the pod is not found, already delete
// 503    : the service is not able to answer now, potentially not reaching the leader, you should retry
// 500    : server error, that could be a transient error, retry couple of times
func (d *APIDrainer) evictWithOperatorAPI(ctx context.Context, url string, node *core.Node, pod *core.Pod, abort <-chan struct{}) error {
	span, ctx := tracer.StartSpanFromContext(ctx, "evictWithKubernetesAPI")
	defer span.Finish()

	conditions := GetConditionsTypes(GetNodeOffendingConditions(node, d.globalConfig.SuppliedConditions))
	d.l.Info("using custom eviction endpoint", zap.String("pod", pod.Namespace+"/"+pod.Name), zap.String("endpoint", url))
	maxRetryOn500 := 4
	return d.evictionSequence(ctx, node, pod, abort,
		// eviction function
		func() error {

			logger := d.l.With(zap.String("node", node.Name)).With(zap.String("pod", pod.Namespace+"/"+pod.Name))
			evictionPayload := &policy.Eviction{
				ObjectMeta: meta.ObjectMeta{Namespace: pod.GetNamespace(), Name: pod.GetName(),
					Annotations: map[string]string{EvictionNodeConditionsAnnotationKey: strings.Join(conditions, ",")}},
			}

			var client *http.Client
			urlParsed, err := url2.Parse(url)
			if err != nil {
				logger.Info("custom eviction endpoint response error, can't parse URL", zap.Error(err))
				return EvictionEndpointError{}
			}

			// building the base roundTripper
			var roundTripper http.RoundTripper
			if urlParsed.Scheme == "https" {
				roundTripper = &http.Transport{
					TLSClientConfig: &tls.Config{
						// We are not trying to verify the server side for the moment
						// Men in the middle risk is low if not null: CNP helps here.
						// We can add more verification later if needed
						InsecureSkipVerify: true,
					},
				}
			} else {
				roundTripper = http.DefaultTransport
			}

			// If the user specify a parameter "token-audience" on the URL then we will forge a token that is using the value of the parameter as audience in the token
			// If the user do not specify the audience then that means that he does not want the token (audience is mandatory)
			tokenAudience := urlParsed.Query().Get("token-audience")
			if tokenAudience != "" {
				// Uses Emissary to get JWTs.
				roundTripper = authnclient.NewRoundTripper(roundTripper, authnclient.NewEmissaryTokenGetter(tokenAudience))
				// Removing this token parameter so that the server don't get it on the URL. The value is now available for the server inside the bearer token
				urlParsed.Query().Del("token-audience")
				logger.Info("Using token-audience parameter", zap.String("token-audience", tokenAudience))
			}

			client = &http.Client{Transport: roundTripper, Timeout: 20 * time.Second}
			logger.Info("calling eviction++", zap.String("url", urlParsed.String()))
			req, err := http.NewRequest("POST", urlParsed.String(), GetEvictionJsonPayload(evictionPayload))
			req = req.WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")

			client = httptrace.WrapClient(client)
			resp, err := client.Do(req)
			if err != nil {
				logger.Info("custom eviction endpoint response error", zap.Error(err))
				if tokenAudience != "" && strings.Contains(err.Error(), "unable to retrieve token from vault (http status: 400)") {
					return AudienceNotFoundError{Audience: tokenAudience}
				}
				if os.IsTimeout(err) {
					return EvictionEndpointError{IsRequestTimeout: true}
				}
				return EvictionEndpointError{}
			}
			defer resp.Body.Close()
			logger.Info("custom eviction endpoint response", zap.String("endpoint", url), zap.Int("responseCode", resp.StatusCode))
			switch {
			case resp.StatusCode == http.StatusOK:
				return nil
			case resp.StatusCode == http.StatusTooManyRequests:
				return apierrors.NewTooManyRequests("retry later", 10)
			case resp.StatusCode == http.StatusNotFound:
				return apierrors.NewNotFound(schema.GroupResource{Resource: "pod"}, pod.Name)
			case resp.StatusCode == http.StatusServiceUnavailable:
				return apierrors.NewTooManyRequests("retry later, service endpoint is not the leader", 15)
			case resp.StatusCode == http.StatusInternalServerError:
				respContent, _ := ioutil.ReadAll(resp.Body)
				if maxRetryOn500 > 0 {
					maxRetryOn500--
					logger.Info("Custom eviction endpoint returned an error", zap.Int("code", resp.StatusCode), zap.String("body", string(respContent)))
					return apierrors.NewTooManyRequests("retry later following endpoint error", 20)
				}
				logger.Error("Too many service error from custom eviction endpoint.", zap.Int("code", resp.StatusCode), zap.String("body", string(respContent)))
				return EvictionEndpointError{StatusCode: resp.StatusCode, AfterSeveralRetries: true}
			default:
				respContent, _ := ioutil.ReadAll(resp.Body)
				logger.Error("Unexpected response code from custom eviction endpoint.", zap.Int("code", resp.StatusCode), zap.String("body", string(respContent)))
				return EvictionEndpointError{StatusCode: resp.StatusCode}
			}
		},
		// error handling function
		func(err error) error {
			return err
		},
	)
}

func (d *APIDrainer) evictionSequence(ctx context.Context, node *core.Node, pod *core.Pod, abort <-chan struct{}, evictionFunc func() error, otherErrorsHandlerFunc func(e error) error) error {
	span, ctx := tracer.StartSpanFromContext(ctx, "evictionSequence")
	defer span.Finish()

	// we will retry eviction till minEvictionTimeout (or podTerminationGracePeriod if it is bigger), augmented by evictionHeadroom
	ctx, cancel := context.WithTimeout(ctx, d.getMinEvictionTimeoutWithEvictionHeadRoom(pod))
	defer cancel()
	backoff := wait.Backoff{
		Duration: 10 * time.Second,
		Factor:   1.5,
		Jitter:   0,
		Steps:    100, // we want the max backoff for a single step controlled by cap, not steps, so set steps arbitrarily large to effectively ignore it
		Cap:      time.Minute,
	}
	for {
		select {
		case <-abort:
			return errors.New("pod eviction aborted")
		case <-ctx.Done():
			_, ok := GetAnnotationFromPodOrController(EvictionAPIURLAnnotationKey, pod, d.runtimeObjectStore)
			return PodEvictionTimeoutError{isEvictionPP: ok} // this one is typed because we match it to a failure cause
		default:
			pvcs, err := d.getInScopePVCs(ctx, pod)
			if err != nil {
				d.l.Error("Cannot fetch pod pvc's", zap.Error(err), zap.String("pod", pod.Name))
				continue
			}

			// If you try to evict a terminating pod (e.g., retry to evict a pod that's
			// already been evicted but is still terminating because of a finalizer), and
			// the pod disruption budget is currently exhausted (e.g., because said pod is
			// terminating and its replacement is still pending), the eviction API naively
			// responds with a 429: "Cannot evict pod as it would violate the pod's
			// disruption budget," instead of something less misleading (it actually
			// wouldn't violate the disruption budget, because it wouldn't change the number
			// of available pods). Later, when the replacement pod is running, if you try to
			// re-evict the terminating pod, the eviction API responds with a 201. To avoid
			// the confusion, let's not even try to evict terminating pods, because that
			// doesn't make much sense anyway. However, we still want to wait for their
			// deletion, which is why we filter here and not in GetPodsToDrain.
			if pod.DeletionTimestamp == nil {
				err = evictionFunc()
			}
			switch {
			// The eviction API returns 429 Too Many Requests if a pod
			// cannot currently be evicted, for example due to a pod
			// disruption budget.
			case apierrors.IsTooManyRequests(err):
				d.l.Info("received 429 while evicting pod", zap.String("node", node.Name), zap.String("pod", pod.Name), zap.String("pod_namespace", pod.Namespace), zap.Error(err))
				d.eventRecorder.NodeEventf(ctx, node, core.EventTypeWarning, eventReasonEvictionAttemptFailed, "Attempt to evict pod %s/%s failed: %v", pod.Namespace, pod.Name, err)
				d.eventRecorder.PodEventf(ctx, pod, core.EventTypeWarning, eventReasonEvictionAttemptFailed, "Attempt to evict pod from node %s failed: %v", node.Name, err)
				waitTime := backoff.Step()
				if statErr, ok := err.(apierrors.APIStatus); ok && statErr.Status().Details != nil {
					if proposedWaitSeconds := statErr.Status().Details.RetryAfterSeconds; proposedWaitSeconds > 0 {
						waitTime = time.Duration(proposedWaitSeconds) * time.Second
					}
				}
				select {
				case <-time.After(waitTime):
				case <-ctx.Done():
				}
			case apierrors.IsNotFound(err):
				// the pod is already gone
				// maybe we still need to perform PVC management
				err = d.deletePVCAndPV(ctx, pod, pvcs)
				if err != nil {
					return VolumeCleanupError{Err: err} // this one is typed because we match it to a failure cause
				}
				return nil
			case err != nil:
				if eh := otherErrorsHandlerFunc(err); eh != nil {
					return eh
				}
			default: // this means the API answered 200/201, we wait for the pod deletion
				// now that the eviction is confirmed we can only wait for the pod terminationGracePeriod (and evictionHeadroom to give some buffer)
				err := d.awaitDeletion(ctx, pod, d.getGracePeriodWithEvictionHeadRoom(pod))
				if err != nil {
					return fmt.Errorf("cannot confirm pod was deleted: %w", err)
				}
				err = d.deletePVCAndPV(ctx, pod, pvcs)
				if err != nil {
					return VolumeCleanupError{Err: err} // this one is typed because we match it to a failure cause
				}
				return nil
			}
		}
	}
}

func (d *APIDrainer) awaitDeletion(ctx context.Context, pod *core.Pod, timeout time.Duration) error {
	// We need to optimise the pollPeriod to maximize the chance to capture the deletion and not falling into rate limiting issue on the client side
	pollPeriod := timeout / 10 // let's make 10 tentatives to check deletion
	if pollPeriod < 6*time.Second {
		pollPeriod = 6 * time.Second
	}
	if pollPeriod > 2*time.Minute {
		pollPeriod = 2 * time.Minute
	}

	polls := 0
	err := wait.PollImmediate(pollPeriod, timeout, func() (bool, error) {
		polls += 1
		var got core.Pod
		err := d.crClient.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}, &got)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, fmt.Errorf("cannot get pod %s/%s: %w", pod.GetNamespace(), pod.GetName(), err)
		}
		if got.GetUID() != pod.GetUID() {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		if errors.Is(err, wait.ErrWaitTimeout) {
			d.l.With(zap.String("pod", pod.Namespace+"/"+pod.Name), zap.Duration("timeout", timeout), zap.Duration("poll", pollPeriod), zap.Int("polls", polls)).
				Warn("pod deletion timed out")
			return PodDeletionTimeoutError{} // this one is typed because we match it to a failure cause
		}
		return err // unexpected Get error above
	}
	return nil
}

func (d *APIDrainer) deletePVCAndPV(ctx context.Context, pod *core.Pod, pvcs []*core.PersistentVolumeClaim) error {
	span, ctx := tracer.StartSpanFromContext(ctx, "deletePVCAndPV")
	defer span.Finish()
	span.SetTag("pod", pod.GetName())

	pvcDeleted, err := d.deletePVCAssociatedWithStorageClass(ctx, pod, pvcs)
	if err != nil {
		return err
	}

	// now if the pod is pending because it is missing the PVC and if it is controlled by a statefulset we should delete it to have statefulset controller rebuilding the PVC
	if len(pvcDeleted) > 0 {
		if err := d.deletePVAssociatedWithDeletedPVC(ctx, pod, pvcDeleted); err != nil {
			return err
		}
		for _, pvc := range pvcDeleted {
			if err := d.podDeleteRetryWaitingForPVC(ctx, pod, pvc); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *APIDrainer) podDeleteRetryWaitingForPVC(ctx context.Context, pod *core.Pod, pvc *core.PersistentVolumeClaim) error {
	span, ctx := tracer.StartSpanFromContext(ctx, "podDeleteRetryWaitingForPVC")
	defer span.Finish()
	span.SetTag("pvc", pvc.GetName())

	podDeleteCheckPVCFunc := func() (bool, error) {
		// check if the PVC was created
		gotPVC, err := d.c.CoreV1().PersistentVolumeClaims(pvc.GetNamespace()).Get(ctx, pvc.GetName(), meta.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}

		if !apierrors.IsNotFound(err) {
			if gotPVC != nil && string(gotPVC.UID) != "" && string(gotPVC.UID) != string(pvc.UID) {
				d.l.Info("associated pvc was recreated", zap.String("pod", pod.GetName()), zap.String("namespace", pod.GetNamespace()), zap.String("pvc", pvc.GetName()), zap.String("pvc-old-uid", string(pvc.GetUID())), zap.String("pvc-new-uid", string(gotPVC.GetUID())))
				return true, nil
			}
		}

		d.l.Info("deleting pod to force pvc recreate", zap.String("pod", pod.GetName()), zap.String("namespace", pod.GetNamespace()))
		err = d.c.CoreV1().Pods(pod.GetNamespace()).Delete(ctx, pod.GetName(), meta.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("cannot delete pod %s/%s to regenerated PVC: %w", pod.GetNamespace(), pod.GetName(), err)
		}
		return false, nil
	}
	return wait.PollImmediate(DefaultPodDeletePeriodWaitingForPVC, DefaultPVCRecreateTimeout, podDeleteCheckPVCFunc)
}

func (d *APIDrainer) deletePVAssociatedWithDeletedPVC(ctx context.Context, pod *core.Pod, pvcDeleted []*core.PersistentVolumeClaim) error {
	span, ctx := tracer.StartSpanFromContext(ctx, "deletePVAssociatedWithDeletedPVC")
	defer span.Finish()

	for _, claim := range pvcDeleted {
		if claim.Spec.VolumeName == "" {
			continue
		}
		var pv core.PersistentVolume
		err := d.crClient.Get(ctx, types.NamespacedName{Name: claim.Spec.VolumeName}, &pv)
		if apierrors.IsNotFound(err) {
			d.l.Info("GET: PV not found", zap.String("name", claim.Spec.VolumeName), zap.String("claim", claim.Name), zap.String("claimNamespace", claim.Namespace))
			continue // This PV was already deleted
		}

		d.eventRecorder.PersistentVolumeEventf(ctx, &pv, core.EventTypeNormal, "Eviction", fmt.Sprintf("Deletion requested due to association with evicted pvc %s/%s and pod %s/%s", claim.Namespace, claim.Name, pod.Namespace, pod.Name))
		d.eventRecorder.PodEventf(ctx, pod, core.EventTypeNormal, "Eviction", fmt.Sprintf("Deletion of associated PV %s", pv.Name))

		err = d.c.CoreV1().PersistentVolumes().Delete(ctx, pv.Name, meta.DeleteOptions{})
		if apierrors.IsNotFound(err) {
			d.l.Info("DELETE: PV not found", zap.String("name", pv.Name))
			continue // This PV was already deleted
		}
		if err != nil {
			d.eventRecorder.PersistentVolumeEventf(ctx, &pv, core.EventTypeWarning, "EvictionFailure", fmt.Sprintf("Could not delete PV: %v", err))
			d.eventRecorder.PodEventf(ctx, pod, core.EventTypeWarning, "EvictionFailure", fmt.Sprintf("Could not delete PV %s: %v", pv.Name, err))
			return fmt.Errorf("cannot delete pv %s: %w", pv.Name, err)
		}
		d.l.Info("deleting pv", zap.String("pv", pv.Name))

		// wait for PVC complete deletion
		if err := d.awaitPVDeletion(ctx, &pv, time.Minute); err != nil {
			return fmt.Errorf("pv deletion timeout %s: %w", pv.Name, err)
		}
	}
	return nil
}

func (d *APIDrainer) awaitPVDeletion(ctx context.Context, pv *core.PersistentVolume, timeout time.Duration) error {
	// We need to optimise the pollPeriod to maximize the chance to capture the deletion and not falling into rate limiting issue on the client side
	pollPeriod := timeout / 10 // let's make 10 tentatives to check deletion
	if pollPeriod < 6*time.Second {
		pollPeriod = 6 * time.Second
	}
	if pollPeriod > 2*time.Minute {
		pollPeriod = 2 * time.Minute
	}

	return wait.PollImmediate(pollPeriod, timeout, func() (bool, error) {
		var got core.PersistentVolume
		err := d.crClient.Get(ctx, types.NamespacedName{Name: pv.Name}, &got)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, fmt.Errorf("cannot get pv %s: %w", pv.GetName(), err)
		}
		if string(got.GetUID()) != string(pv.GetUID()) {
			return true, nil
		}
		return false, nil
	})
}

func PVCStorageClassCleanupEnabled(p *v1.Pod, store RuntimeObjectStore, defaultTrueIfNoEvictionUrl bool) bool {
	valAnnotation, _ := GetAnnotationFromPodOrController(PVCStorageClassCleanupAnnotationKey, p, store)
	if valAnnotation == PVCStorageClassCleanupAnnotationTrueValue {
		return true
	}
	if valAnnotation == PVCStorageClassCleanupAnnotationFalseValue {
		return false
	}

	if defaultTrueIfNoEvictionUrl {
		_, evictionUrlFound := GetAnnotationFromPodOrController(EvictionAPIURLAnnotationKey, p, store)
		return !evictionUrlFound
	}

	return false
}

// getInScopePVCs will return all pvcs that are "in scope" and available.
// Where in scope means that the storage class is allowed to be deleted by configuration.
func (d *APIDrainer) getInScopePVCs(ctx context.Context, pod *core.Pod) ([]*core.PersistentVolumeClaim, error) {
	span, ctx := tracer.StartSpanFromContext(ctx, "fetchPVCsAssociatedWithPod")
	defer span.Finish()

	if d.storageClassesAllowingPVDeletion == nil {
		return nil, nil
	}

	if !PVCStorageClassCleanupEnabled(pod, d.runtimeObjectStore, d.globalConfig.PVCManagementEnableIfNoEvictionUrl) {
		return nil, nil
	}

	claims := []*core.PersistentVolumeClaim{}
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		d.l.Info("looking at volume with PVC", zap.String("name", v.Name), zap.String("claim", v.PersistentVolumeClaim.ClaimName))
		pvc, err := d.c.CoreV1().PersistentVolumeClaims(pod.GetNamespace()).Get(ctx, v.PersistentVolumeClaim.ClaimName, meta.GetOptions{})
		if apierrors.IsNotFound(err) {
			d.l.Info("GET: PVC not found", zap.String("name", v.Name), zap.String("claim", v.PersistentVolumeClaim.ClaimName))
			continue // This PVC was already deleted
		}
		if err != nil {
			return nil, fmt.Errorf("cannot get pvc %s/%s: %w", pod.GetNamespace(), v.PersistentVolumeClaim.ClaimName, err)
		}
		if pvc.Spec.StorageClassName == nil {
			d.l.Info("PVC with no StorageClassName", zap.String("claim", v.PersistentVolumeClaim.ClaimName))
			continue
		}
		if _, ok := d.storageClassesAllowingPVDeletion[*pvc.Spec.StorageClassName]; !ok {
			d.l.Info("Skipping StorageClassName", zap.String("storageClassName", *pvc.Spec.StorageClassName))
			continue
		}

		claims = append(claims, pvc)
	}
	return claims, nil
}

// deletePVCAssociatedWithStorageClass takes care of deleting the PVCs associated with the annotated classes
// returns the list of deleted PVCs and the first error encountered if any
func (d *APIDrainer) deletePVCAssociatedWithStorageClass(ctx context.Context, pod *core.Pod, pvcs []*core.PersistentVolumeClaim) ([]*core.PersistentVolumeClaim, error) {
	span, ctx := tracer.StartSpanFromContext(ctx, "deletePVCAssociatedWithStorageClass")
	defer span.Finish()

	deletedPVCs := []*core.PersistentVolumeClaim{}
	for _, pvc := range pvcs {
		var freshPvc core.PersistentVolumeClaim
		err := d.crClient.Get(ctx, types.NamespacedName{Namespace: pvc.Namespace, Name: pvc.Name}, &freshPvc)
		if apierrors.IsNotFound(err) {
			d.l.Info("DELETE: PVC not found", zap.String("claim", pvc.Name))
			continue // This PVC was already deleted
		}
		if pvc.UID != freshPvc.UID {
			d.l.Info("DELETE: PVC already replaced", zap.String("claim", pvc.Name))
			continue
		}

		d.eventRecorder.PodEventf(ctx, pod, core.EventTypeNormal, "Eviction", fmt.Sprintf("Deletion of associated PVC %s/%s", pvc.Namespace, pvc.Name))
		d.eventRecorder.PersistentVolumeClaimEventf(ctx, pvc, core.EventTypeNormal, "Eviction", fmt.Sprintf("Deletion requested due to association with evicted pod %s/%s", pod.Namespace, pod.Name))

		err = d.c.CoreV1().PersistentVolumeClaims(pod.GetNamespace()).Delete(ctx, pvc.Name, meta.DeleteOptions{})
		if apierrors.IsNotFound(err) {
			d.l.Info("DELETE: PVC not found", zap.String("claim", pvc.Name))
			continue // This PVC was already deleted
		}
		if err != nil {
			d.eventRecorder.PodEventf(ctx, pod, core.EventTypeWarning, "EvictionFailure", fmt.Sprintf("Could not delete PVC %s/%s: %v", pvc.Namespace, pvc.Name, err))
			d.eventRecorder.PersistentVolumeClaimEventf(ctx, pvc, core.EventTypeWarning, "EvictionFailure", fmt.Sprintf("Could not delete: %v", err))
			return deletedPVCs, fmt.Errorf("cannot delete pvc %s/%s: %w", pod.GetNamespace(), pvc.Name, err)
		}
		d.l.Info("deleting pvc", zap.String("pvc", pvc.Name), zap.String("namespace", pod.GetNamespace()), zap.String("pvc-uid", string(pvc.GetUID())))

		// wait for PVC complete deletion
		if err := d.awaitPVCDeletion(ctx, pvc, awaitPVCDeletionTimeout); err != nil {
			return deletedPVCs, fmt.Errorf("pvc deletion timeout %s/%s: %w", pod.GetNamespace(), pvc.Name, err)
		}
		deletedPVCs = append(deletedPVCs, pvc)
	}
	return deletedPVCs, nil
}

func (d *APIDrainer) awaitPVCDeletion(ctx context.Context, pvc *core.PersistentVolumeClaim, timeout time.Duration) error {
	return wait.PollImmediate(1*time.Second, timeout, func() (bool, error) {
		d.l.Info("waiting for pvc complete deletion", zap.String("pvc", pvc.Name), zap.String("namespace", pvc.GetNamespace()), zap.String("pvc-uid", string(pvc.GetUID())))
		var got core.PersistentVolumeClaim
		err := d.crClient.Get(ctx, types.NamespacedName{Namespace: pvc.Namespace, Name: pvc.Name}, &got)
		if apierrors.IsNotFound(err) {
			d.l.Info("pvc not found. It is deleted.", zap.String("pvc", pvc.Name), zap.String("namespace", pvc.GetNamespace()), zap.String("pvc-uid", string(pvc.GetUID())))
			return true, nil
		}
		if err != nil {
			return false, fmt.Errorf("cannot get pvc %s/%s: %w", pvc.GetNamespace(), pvc.GetName(), err)
		}
		if string(got.GetUID()) != string(pvc.GetUID()) {
			d.l.Info("pvc found but with different UID. It is deleted.", zap.String("pvc", pvc.Name), zap.String("namespace", pvc.GetNamespace()), zap.String("pvc-uid", string(pvc.GetUID())), zap.String("pvc-new-uid", string(got.GetUID())))
			return true, nil
		}
		d.l.Info("pvc still present", zap.String("pvc", pvc.Name), zap.String("namespace", pvc.GetNamespace()), zap.String("pvc-uid", string(pvc.GetUID())))
		return false, nil
	})
}

func (d *APIDrainer) performNodeReplacement(ctx context.Context, n *core.Node, reason string) error {
	span, ctx := tracer.StartSpanFromContext(ctx, "performNodeReplacement")
	defer span.Finish()

	fresh, err := d.c.CoreV1().Nodes().Get(ctx, n.GetName(), meta.GetOptions{})
	if err != nil {
		return fmt.Errorf("cannot get node %s: %w", n.GetName(), err)
	}
	fresh.Labels[NodeLabelKeyReplaceRequest] = NodeLabelValueReplaceRequested
	if _, err := d.c.CoreV1().Nodes().Update(ctx, fresh, meta.UpdateOptions{FieldManager: "draino"}); err != nil {
		return fmt.Errorf("cannot request replacement node %s: %w", fresh.GetName(), err)
	}
	tags, _ := tag.New(context.Background(), tag.Upsert(TagNodeName, n.GetName()), tag.Upsert(TagReason, reason)) // nolint:gosec
	StatRecordForNode(tags, n, MeasureNodesReplacementRequest.M(1))
	return nil
}

func (d *APIDrainer) ReplaceNode(ctx context.Context, n *core.Node) (bool, error) {
	span, ctx := tracer.StartSpanFromContext(ctx, "ReplaceNode")
	defer span.Finish()
	err := d.performNodeReplacement(ctx, n, newNodeRequestReasonReplacement)
	return err != nil, err
}

func (d *APIDrainer) PreprovisionNode(ctx context.Context, n *core.Node) error {
	span, ctx := tracer.StartSpanFromContext(ctx, "PreprovisionNode")
	defer span.Finish()

	return d.performNodeReplacement(ctx, n, newNodeRequestReasonPreprovisioning)
}

func (d *APIDrainer) GetReplacementStatus(ctx context.Context, n *core.Node) (NodeReplacementStatus, error) {
	freshNode, err := d.runtimeObjectStore.Nodes().Get(n.Name)
	if err != nil {
		return "", err
	}
	return NodeReplacementStatus(freshNode.Labels[NodeLabelKeyReplaceRequest]), nil
}

var evictionPayloadEncoder runtime.Encoder

func GetEvictionPayloadEncoder() runtime.Encoder {
	if evictionPayloadEncoder != nil {
		return evictionPayloadEncoder
	}
	scheme := runtime.NewScheme()
	policy.SchemeBuilder.AddToScheme(scheme)
	codecFactory := serializer.NewCodecFactory(scheme)
	jsonSerializer := runtimejson.NewSerializerWithOptions(runtimejson.DefaultMetaFactory, scheme, scheme, runtimejson.SerializerOptions{})
	evictionPayloadEncoder = codecFactory.WithoutConversion().EncoderForVersion(jsonSerializer, policy.SchemeGroupVersion)
	return evictionPayloadEncoder
}

func GetEvictionJsonPayload(obj *policy.Eviction) *bytes.Buffer {
	buffer := bytes.NewBuffer([]byte{})
	GetEvictionPayloadEncoder().Encode(obj, buffer)
	return buffer
}
