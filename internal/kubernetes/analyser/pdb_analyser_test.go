package analyser

import (
	"context"
	"testing"
	"time"

	"k8s.io/utils/clock"

	"github.com/go-logr/zapr"
	"go.uber.org/zap"

	"github.com/planetlabs/draino/internal/kubernetes/index"
	"github.com/planetlabs/draino/internal/kubernetes/k8sclient"
	"github.com/stretchr/testify/assert"
	"golang.org/x/exp/slices"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestPDBAnalyser(t *testing.T) {
	labelsOne := map[string]string{"matching": "labels", "set": "one"}
	labelsTwo := map[string]string{"matching": "labels", "set": "two"}
	tests := []struct {
		Name     string
		NodeName string
		Expected []struct {
			PodName   string
			PDBName   string
			Namespace string
		}
		Objects []runtime.Object
	}{
		{
			Name:     "Should find one pod",
			NodeName: "my-node",
			Expected: []struct {
				PodName   string
				PDBName   string
				Namespace string
			}{
				{PodName: "failing-pod-1", PDBName: "my-pdb", Namespace: "default"},
			},
			Objects: []runtime.Object{
				createNode("my-node"),
				createPod("running-pod-1", "default", "my-node", true, labelsOne),
				createPod("failing-pod-1", "default", "my-node", false, labelsOne),
				createPDB("my-pdb", "default", labelsOne),
			},
		},
		{
			Name:     "Should find two pods with different PDBs in same namespace",
			NodeName: "my-node",
			Expected: []struct {
				PodName   string
				PDBName   string
				Namespace string
			}{
				{PodName: "failing-pod-1", PDBName: "my-pdb-1", Namespace: "default"},
				{PodName: "failing-pod-2", PDBName: "my-pdb-2", Namespace: "default"},
			},
			Objects: []runtime.Object{
				createNode("my-node"),
				createPod("running-pod-1", "default", "my-node", true, labelsOne),
				createPod("failing-pod-1", "default", "my-node", false, labelsOne),
				createPod("failing-pod-2", "default", "my-node", false, labelsTwo),
				createPDB("my-pdb-1", "default", labelsOne),
				createPDB("my-pdb-2", "default", labelsTwo),
			},
		},
		{
			Name:     "Should find two pods with different PDBs in different namespaces",
			NodeName: "my-node",
			Expected: []struct {
				PodName   string
				PDBName   string
				Namespace string
			}{
				{PodName: "failing-pod", PDBName: "my-pdb", Namespace: "default"},
				{PodName: "failing-pod", PDBName: "my-pdb", Namespace: "kube-system"},
			},
			Objects: []runtime.Object{
				createNode("my-node"),
				createPod("running-pod-1", "default", "my-node", true, labelsOne),
				createPod("failing-pod", "default", "my-node", false, labelsOne),
				createPod("failing-pod", "kube-system", "my-node", false, labelsTwo),
				createPDB("my-pdb", "default", labelsOne),
				createPDB("my-pdb", "kube-system", labelsTwo),
			},
		},
		{
			Name:     "Should find one only one pod even tow exist in different namespaces",
			NodeName: "my-node",
			Expected: []struct {
				PodName   string
				PDBName   string
				Namespace string
			}{
				{PodName: "failing-pod", PDBName: "my-pdb", Namespace: "kube-system"},
			},
			Objects: []runtime.Object{
				createNode("my-node"),
				createPod("running-pod-1", "default", "my-node", true, labelsOne),
				createPod("running-pod-2", "default", "my-node", true, labelsOne),
				createPod("failing-pod", "kube-system", "my-node", false, labelsTwo),
				createPDB("my-pdb", "default", labelsOne),
				createPDB("my-pdb", "kube-system", labelsTwo),
			},
		},
		{
			Name:     "Should return empty list if none were found",
			NodeName: "my-node",
			Expected: []struct {
				PodName   string
				PDBName   string
				Namespace string
			}{},
			Objects: []runtime.Object{
				createNode("my-node"),
				createPod("running-pod-1", "default", "my-node", true, labelsOne),
				createPod("running-pod-2", "default", "my-node", true, labelsOne),
				createPod("running-pod-3", "kube-system", "my-node", true, labelsTwo),
				createPDB("my-pdb", "default", labelsOne),
				createPDB("my-pdb", "kube-system", labelsTwo),
			},
		},
		{
			Name:     "Should not find failing pods of other nodes",
			NodeName: "my-node",
			Expected: []struct {
				PodName   string
				PDBName   string
				Namespace string
			}{},
			Objects: []runtime.Object{
				createNode("my-node"),
				createNode("my-node-2"),
				createPod("running-pod-1", "default", "my-node", true, labelsOne),
				createPod("running-pod-2", "default", "my-node-2", true, labelsOne),
				createPod("failing-pod-1", "default", "my-node-2", false, labelsOne),
				createPDB("my-pdb", "default", labelsOne),
				createPDB("my-pdb", "kube-system", labelsTwo),
			},
		},
	}

	testLogger := zapr.NewLogger(zap.NewNop())
	for _, tt := range tests {
		t.Run(tt.Name, func(t *testing.T) {
			wrapper, err := k8sclient.NewFakeClient(k8sclient.FakeConf{Objects: tt.Objects})
			assert.NoError(t, err)

			ctx, cancelFn := context.WithCancel(context.Background())
			defer cancelFn()
			indexer, err := index.New(ctx, wrapper.GetManagerClient(), wrapper.GetCache(), testLogger)
			assert.NoError(t, err)

			ch := make(chan struct{})
			defer close(ch)
			wrapper.Start(ch)

			analyser := NewPDBAnalyser(context.Background(), zapr.NewLogger(zap.NewNop()), indexer, clock.RealClock{}, time.Second)
			pods, err := analyser.BlockingPodsOnNode(context.Background(), tt.NodeName)
			assert.NoError(t, err)

			assert.Equal(t, len(tt.Expected), len(pods), "received amount of pods to not match expected amount")
			for _, exp := range tt.Expected {
				idx := slices.IndexFunc(pods, func(blocking BlockingPod) bool {
					return blocking.Pod.GetName() == exp.PodName && blocking.Pod.GetNamespace() == exp.Namespace
				})
				assert.Greater(t, idx, -1, "cannot find expected pod in list")
				assert.Equal(t, exp.PodName, pods[idx].Pod.GetName())
				assert.Equal(t, exp.PDBName, pods[idx].PDB.GetName())
			}
		})
	}
}

func TestPDBAnalyser_IsPDBBlocked(t *testing.T) {
	tests := []struct {
		Name      string
		IsBlocked bool
		Pod       *corev1.Pod
		PDB       *policyv1.PodDisruptionBudget
	}{
		{
			Name:      "Should succeed if PDB has budget",
			IsBlocked: false,
			Pod:       createPodWithStatus(true),
			PDB:       createPDBWithStatus(1, 3),
		},
		{
			Name:      "Should succeed as pod is taking the budget",
			IsBlocked: false,
			Pod:       createPodWithStatus(false),
			PDB:       createPDBWithStatus(1, 1),
		},
		{
			Name:      "Should fail because there is no budget left",
			IsBlocked: true,
			Pod:       createPodWithStatus(true),
			PDB:       createPDBWithStatus(1, 0),
		},
	}

	for _, tt := range tests {
		t.Run(tt.Name, func(t *testing.T) {
			res := IsPDBBlockedByPod(context.Background(), tt.Pod, tt.PDB)
			assert.Equal(t, tt.IsBlocked, res)
		})
	}
}

func createNode(name string) *corev1.Node {
	return &corev1.Node{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Node",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
}

func createPDB(name, ns string, selector labels.Set) *policyv1.PodDisruptionBudget {
	return &policyv1.PodDisruptionBudget{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "policy/v1",
			Kind:       "PodDisruptionBudget",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: selector,
			},
		},
	}
}

func createPod(name, ns, nodeName string, isReady bool, ls labels.Set) *corev1.Pod {
	ready := corev1.ConditionFalse
	if isReady {
		ready = corev1.ConditionTrue
	}
	return &corev1.Pod{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Pod",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    ls,
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.ContainersReady,
					Status: ready,
				},
			},
		},
	}
}

type pdbStatus struct {
	Ls      labels.Set
	Des     int32
	Healthy int32
}

func createPDBWithStatus(des, healthy int32) *policyv1.PodDisruptionBudget {
	pdb := createPDB("test-pdb", "default", map[string]string{})
	pdb.Status.DesiredHealthy = des
	pdb.Status.CurrentHealthy = healthy
	return pdb
}

func createPodWithStatus(isReady bool) *corev1.Pod {
	return createPod("test", "test", "test", isReady, map[string]string{})
}
