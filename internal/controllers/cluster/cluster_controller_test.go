/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cluster

import (
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	utilfeature "k8s.io/component-base/featuregate/testing"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimev1 "sigs.k8s.io/cluster-api/api/runtime/v1beta2"
	"sigs.k8s.io/cluster-api/feature"
	"sigs.k8s.io/cluster-api/internal/contract"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/collections"
	"sigs.k8s.io/cluster-api/util/conditions"
	v1beta1conditions "sigs.k8s.io/cluster-api/util/conditions/deprecated/v1beta1"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/test/builder"
)

const (
	clusterReconcileNamespace = "test-cluster-reconcile"
)

func TestClusterReconciler(t *testing.T) {
	ns, err := env.CreateNamespace(ctx, clusterReconcileNamespace)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := env.Delete(ctx, ns); err != nil {
			t.Fatal(err)
		}
	}()

	t.Run("Should create a Cluster", func(t *testing.T) {
		g := NewWithT(t)

		instance := &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test1-",
				Namespace:    ns.Name,
			},
			Spec: clusterv1.ClusterSpec{
				ControlPlaneRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: builder.ControlPlaneGroupVersion.Group,
					Kind:     builder.GenericControlPlaneKind,
					Name:     "cp1",
				},
			},
		}

		// Create the Cluster object and expect the Reconcile and Deployment to be created
		g.Expect(env.Create(ctx, instance)).To(Succeed())
		key := client.ObjectKey{Namespace: instance.Namespace, Name: instance.Name}
		defer func() {
			err := env.Delete(ctx, instance)
			g.Expect(err).ToNot(HaveOccurred())
		}()

		// Make sure the Cluster exists.
		g.Eventually(func() bool {
			if err := env.Get(ctx, key, instance); err != nil {
				return false
			}
			return len(instance.Finalizers) > 0
		}, timeout).Should(BeTrue())

		// Validate the RemoteConnectionProbe condition is false (because kubeconfig Secret doesn't exist)
		g.Eventually(func(g Gomega) {
			g.Expect(env.Get(ctx, key, instance)).To(Succeed())

			condition := conditions.Get(instance, clusterv1.ClusterRemoteConnectionProbeCondition)
			g.Expect(condition).ToNot(BeNil())
			g.Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(condition.Reason).To(Equal(clusterv1.ClusterRemoteConnectionProbeFailedReason))
		}, timeout).Should(Succeed())

		t.Log("Creating the Cluster Kubeconfig Secret")
		g.Expect(env.CreateKubeconfigSecret(ctx, instance)).To(Succeed())

		g.Eventually(func(g Gomega) {
			g.Expect(env.Get(ctx, key, instance)).To(Succeed())

			condition := conditions.Get(instance, clusterv1.ClusterRemoteConnectionProbeCondition)
			g.Expect(condition).ToNot(BeNil())
			g.Expect(condition.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(condition.Reason).To(Equal(clusterv1.ClusterRemoteConnectionProbeSucceededReason))
		}, timeout).Should(Succeed())
	})

	t.Run("Should successfully patch a cluster object if the status diff is empty but the spec diff is not", func(t *testing.T) {
		g := NewWithT(t)

		// Setup
		cluster := &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test2-",
				Namespace:    ns.Name,
			},
			Spec: clusterv1.ClusterSpec{
				ControlPlaneRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: builder.ControlPlaneGroupVersion.Group,
					Kind:     builder.GenericControlPlaneKind,
					Name:     "cp1",
				},
			},
		}
		g.Expect(env.Create(ctx, cluster)).To(Succeed())
		key := client.ObjectKey{Name: cluster.Name, Namespace: cluster.Namespace}
		defer func() {
			err := env.Delete(ctx, cluster)
			g.Expect(err).ToNot(HaveOccurred())
		}()

		// Wait for reconciliation to happen.
		g.Eventually(func() bool {
			if err := env.Get(ctx, key, cluster); err != nil {
				return false
			}
			return len(cluster.Finalizers) > 0
		}, timeout).Should(BeTrue())

		// Patch
		g.Eventually(func() bool {
			ph, err := patch.NewHelper(cluster, env)
			g.Expect(err).ToNot(HaveOccurred())
			cluster.Spec.InfrastructureRef = clusterv1.ContractVersionedObjectReference{
				APIGroup: builder.InfrastructureGroupVersion.Group,
				Kind:     builder.GenericInfrastructureClusterKind,
				Name:     "test",
			}
			cluster.Spec.ControlPlaneRef = clusterv1.ContractVersionedObjectReference{
				APIGroup: builder.ControlPlaneGroupVersion.Group,
				Kind:     builder.GenericControlPlaneKind,
				Name:     "test-too",
			}
			g.Expect(ph.Patch(ctx, cluster, patch.WithStatusObservedGeneration{})).To(Succeed())
			return true
		}, timeout).Should(BeTrue())

		// Assertions
		g.Eventually(func() bool {
			instance := &clusterv1.Cluster{}
			if err := env.Get(ctx, key, instance); err != nil {
				return false
			}
			return instance.Spec.InfrastructureRef.IsDefined() &&
				instance.Spec.InfrastructureRef.Name == "test"
		}, timeout).Should(BeTrue())
	})

	t.Run("Should successfully patch a cluster object if the spec diff is empty but the status diff is not", func(t *testing.T) {
		g := NewWithT(t)

		// Setup
		cluster := &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test3-",
				Namespace:    ns.Name,
			},
			Spec: clusterv1.ClusterSpec{
				ControlPlaneRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: builder.ControlPlaneGroupVersion.Group,
					Kind:     builder.GenericControlPlaneKind,
					Name:     "cp1",
				},
			},
		}
		g.Expect(env.Create(ctx, cluster)).To(Succeed())
		key := client.ObjectKey{Name: cluster.Name, Namespace: cluster.Namespace}
		defer func() {
			err := env.Delete(ctx, cluster)
			g.Expect(err).ToNot(HaveOccurred())
		}()

		// Wait for reconciliation to happen.
		g.Eventually(func() bool {
			if err := env.Get(ctx, key, cluster); err != nil {
				return false
			}
			return len(cluster.Finalizers) > 0
		}, timeout).Should(BeTrue())

		// Patch
		g.Eventually(func() bool {
			ph, err := patch.NewHelper(cluster, env)
			g.Expect(err).ToNot(HaveOccurred())
			cluster.Status.Initialization.InfrastructureProvisioned = ptr.To(true)
			g.Expect(ph.Patch(ctx, cluster, patch.WithStatusObservedGeneration{})).To(Succeed())
			return true
		}, timeout).Should(BeTrue())

		// Assertions
		g.Eventually(func() bool {
			instance := &clusterv1.Cluster{}
			if err := env.Get(ctx, key, instance); err != nil {
				return false
			}
			return ptr.Deref(instance.Status.Initialization.InfrastructureProvisioned, false)
		}, timeout).Should(BeTrue())
	})

	t.Run("Should successfully patch a cluster object if the spec diff is empty but the status conditions diff is not", func(t *testing.T) {
		g := NewWithT(t)

		// Setup
		cluster := &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test3-",
				Namespace:    ns.Name,
			},
			Spec: clusterv1.ClusterSpec{
				ControlPlaneRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: builder.ControlPlaneGroupVersion.Group,
					Kind:     builder.GenericControlPlaneKind,
					Name:     "cp1",
				},
			},
		}
		g.Expect(env.Create(ctx, cluster)).To(Succeed())
		key := client.ObjectKey{Name: cluster.Name, Namespace: cluster.Namespace}
		defer func() {
			err := env.Delete(ctx, cluster)
			g.Expect(err).ToNot(HaveOccurred())
		}()

		// Wait for reconciliation to happen.
		g.Eventually(func() bool {
			if err := env.Get(ctx, key, cluster); err != nil {
				return false
			}
			return len(cluster.Finalizers) > 0
		}, timeout).Should(BeTrue())

		// Patch
		g.Eventually(func() bool {
			ph, err := patch.NewHelper(cluster, env)
			g.Expect(err).ToNot(HaveOccurred())
			conditions.Set(cluster, metav1.Condition{
				Type:   clusterv1.ClusterInfrastructureReadyCondition,
				Status: metav1.ConditionTrue,
				Reason: clusterv1.ClusterInfrastructureReadyReason,
			})
			g.Expect(ph.Patch(ctx, cluster, patch.WithStatusObservedGeneration{})).To(Succeed())
			return true
		}, timeout).Should(BeTrue())

		// Assertions
		g.Eventually(func() bool {
			instance := &clusterv1.Cluster{}
			if err := env.Get(ctx, key, instance); err != nil {
				return false
			}
			return conditions.IsTrue(cluster, clusterv1.ClusterInfrastructureReadyCondition)
		}, timeout).Should(BeTrue())
	})

	t.Run("Should successfully patch a cluster object if both the spec diff and status diff are non empty", func(t *testing.T) {
		g := NewWithT(t)

		// Setup
		cluster := &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test4-",
				Namespace:    ns.Name,
			},
			Spec: clusterv1.ClusterSpec{
				ControlPlaneRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: builder.ControlPlaneGroupVersion.Group,
					Kind:     builder.GenericControlPlaneKind,
					Name:     "cp1",
				},
			},
		}

		g.Expect(env.Create(ctx, cluster)).To(Succeed())
		key := client.ObjectKey{Name: cluster.Name, Namespace: cluster.Namespace}
		defer func() {
			err := env.Delete(ctx, cluster)
			g.Expect(err).ToNot(HaveOccurred())
		}()

		// Wait for reconciliation to happen.
		g.Eventually(func() bool {
			if err := env.Get(ctx, key, cluster); err != nil {
				return false
			}
			return len(cluster.Finalizers) > 0
		}, timeout).Should(BeTrue())

		// Patch
		g.Eventually(func() bool {
			ph, err := patch.NewHelper(cluster, env)
			g.Expect(err).ToNot(HaveOccurred())
			cluster.Status.Initialization.InfrastructureProvisioned = ptr.To(true)
			cluster.Spec.InfrastructureRef = clusterv1.ContractVersionedObjectReference{
				APIGroup: builder.InfrastructureGroupVersion.Group,
				Kind:     builder.GenericInfrastructureClusterKind,
				Name:     "test",
			}
			g.Expect(ph.Patch(ctx, cluster, patch.WithStatusObservedGeneration{})).To(Succeed())
			return true
		}, timeout).Should(BeTrue())

		// Assertions
		g.Eventually(func() bool {
			instance := &clusterv1.Cluster{}
			if err := env.Get(ctx, key, instance); err != nil {
				return false
			}
			return ptr.Deref(instance.Status.Initialization.InfrastructureProvisioned, false) &&
				instance.Spec.InfrastructureRef.IsDefined() &&
				instance.Spec.InfrastructureRef.Name == "test"
		}, timeout).Should(BeTrue())
	})

	t.Run("Should re-apply finalizers if removed", func(t *testing.T) {
		g := NewWithT(t)

		// Setup
		cluster := &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test5-",
				Namespace:    ns.Name,
			},
			Spec: clusterv1.ClusterSpec{
				ControlPlaneRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: builder.ControlPlaneGroupVersion.Group,
					Kind:     builder.GenericControlPlaneKind,
					Name:     "cp1",
				},
			},
		}
		g.Expect(env.Create(ctx, cluster)).To(Succeed())
		key := client.ObjectKey{Name: cluster.Name, Namespace: cluster.Namespace}
		defer func() {
			err := env.Delete(ctx, cluster)
			g.Expect(err).ToNot(HaveOccurred())
		}()

		// Wait for reconciliation to happen.
		g.Eventually(func() bool {
			if err := env.Get(ctx, key, cluster); err != nil {
				return false
			}
			return len(cluster.Finalizers) > 0
		}, timeout).Should(BeTrue())

		// Remove finalizers
		g.Eventually(func() bool {
			ph, err := patch.NewHelper(cluster, env)
			g.Expect(err).ToNot(HaveOccurred())
			cluster.SetFinalizers([]string{})
			g.Expect(ph.Patch(ctx, cluster, patch.WithStatusObservedGeneration{})).To(Succeed())
			return true
		}, timeout).Should(BeTrue())

		g.Expect(cluster.Finalizers).Should(BeEmpty())

		// Check finalizers are re-applied
		g.Eventually(func() []string {
			instance := &clusterv1.Cluster{}
			if err := env.Get(ctx, key, instance); err != nil {
				return []string{"not-empty"}
			}
			return instance.Finalizers
		}, timeout).ShouldNot(BeEmpty())
	})

	t.Run("Should successfully set ControlPlaneInitialized on the cluster object if controlplane is ready", func(t *testing.T) {
		g := NewWithT(t)

		ic := builder.InfrastructureCluster(ns.Name, "infracluster1").Build()
		g.Expect(env.CreateAndWait(ctx, ic)).To(Succeed())
		defer func() {
			g.Expect(env.CleanupAndWait(ctx, ic)).To(Succeed())
		}()
		icOriginal := ic.DeepCopy()
		g.Expect(contract.InfrastructureCluster().Provisioned("v1beta2").Set(ic, true)).To(Succeed())
		g.Expect(env.Status().Patch(ctx, ic, client.MergeFrom(icOriginal))).To(Succeed())

		cluster := &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test6-",
				Namespace:    ns.Name,
			},
			Spec: clusterv1.ClusterSpec{
				InfrastructureRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: builder.InfrastructureGroupVersion.Group,
					Kind:     builder.GenericInfrastructureClusterKind,
					Name:     "infracluster1",
				},
			},
		}
		g.Expect(env.Create(ctx, cluster)).To(Succeed())

		key := client.ObjectKey{Name: cluster.Name, Namespace: cluster.Namespace}
		defer func() {
			err := env.Delete(ctx, cluster)
			g.Expect(err).ToNot(HaveOccurred())
		}()
		g.Expect(env.CreateKubeconfigSecret(ctx, cluster)).To(Succeed())

		// Wait for reconciliation to happen.
		g.Eventually(func() bool {
			if err := env.Get(ctx, key, cluster); err != nil {
				return false
			}
			return len(cluster.Finalizers) > 0
		}, timeout).Should(BeTrue())

		// Create a node so we can speed up reconciliation. Otherwise, the machine reconciler will requeue the machine
		// after 10 seconds, potentially slowing down this test.
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "id-node-1",
			},
			Spec: corev1.NodeSpec{
				ProviderID: "aws:///id-node-1",
			},
		}

		g.Expect(env.Create(ctx, node)).To(Succeed())

		machine := &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test6-",
				Namespace:    ns.Name,
				Labels: map[string]string{
					clusterv1.MachineControlPlaneLabel: "",
				},
			},
			Spec: clusterv1.MachineSpec{
				ClusterName: cluster.Name,
				ProviderID:  "aws:///id-node-1",
				InfrastructureRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: builder.InfrastructureGroupVersion.Group,
					Kind:     builder.TestInfrastructureMachineKind,
					Name:     "inframachine",
				},
				Bootstrap: clusterv1.Bootstrap{
					DataSecretName: ptr.To(""),
				},
			},
		}
		machine.Spec.Bootstrap.DataSecretName = ptr.To("test6-bootstrapdata")
		g.Expect(env.Create(ctx, machine)).To(Succeed())
		key = client.ObjectKey{Name: machine.Name, Namespace: machine.Namespace}
		defer func() {
			err := env.Delete(ctx, machine)
			g.Expect(err).ToNot(HaveOccurred())
		}()

		// Wait for machine to be ready.
		//
		// [ncdc] Note, we're using an increased timeout because we've been seeing failures
		// in Prow for this particular block. It looks like it's sometimes taking more than 10 seconds (the value of
		// timeout) for the machine reconciler to add the finalizer and for the change to be persisted to etcd. If
		// we continue to see test timeouts here, that will likely point to something else being the problem, but
		// I've yet to determine any other possibility for the test flakes.
		g.Eventually(func() bool {
			if err := env.Get(ctx, key, machine); err != nil {
				return false
			}
			return len(machine.Finalizers) > 0
		}, timeout*3).Should(BeTrue())

		// Assertion
		key = client.ObjectKey{Name: cluster.Name, Namespace: cluster.Namespace}
		g.Eventually(func() bool {
			if err := env.Get(ctx, key, cluster); err != nil {
				return false
			}
			return conditions.IsTrue(cluster, clusterv1.ClusterControlPlaneInitializedCondition)
		}, timeout).Should(BeTrue())
	})
}

func TestClusterReconciler_reconcileDelete(t *testing.T) {
	utilfeature.SetFeatureGateDuringTest(t, feature.Gates, feature.RuntimeSDK, true)
	utilfeature.SetFeatureGateDuringTest(t, feature.Gates, feature.ClusterTopology, true)

	fakeInfraCluster := builder.InfrastructureCluster("test-ns", "test-cluster").Build()

	tests := []struct {
		name       string
		cluster    *clusterv1.Cluster
		wantDelete bool
	}{
		{
			name: "should proceed with delete if the cluster has the ok-to-delete annotation",
			cluster: func() *clusterv1.Cluster {
				fakeCluster := builder.Cluster("test-ns", "test-cluster").WithTopology(&clusterv1.Topology{ClassRef: clusterv1.ClusterClassRef{Name: "class"}}).WithInfrastructureCluster(fakeInfraCluster).Build()
				if fakeCluster.Annotations == nil {
					fakeCluster.Annotations = map[string]string{}
				}
				fakeCluster.Annotations[runtimev1.OkToDeleteAnnotation] = ""
				return fakeCluster
			}(),
			wantDelete: true,
		},
		{
			name:       "should not proceed with delete if the cluster does not have the ok-to-delete annotation",
			cluster:    builder.Cluster("test-ns", "test-cluster").WithTopology(&clusterv1.Topology{ClassRef: clusterv1.ClusterClassRef{Name: "class"}}).WithInfrastructureCluster(fakeInfraCluster).Build(),
			wantDelete: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			fakeClient := fake.NewClientBuilder().WithObjects(fakeInfraCluster, tt.cluster).Build()
			r := &Reconciler{
				Client:    fakeClient,
				APIReader: fakeClient,
				recorder:  record.NewFakeRecorder(1),
			}

			s := &scope{
				cluster:                 tt.cluster,
				infraCluster:            fakeInfraCluster,
				getDescendantsSucceeded: true,
			}
			_, _ = r.reconcileDelete(ctx, s)
			infraCluster := builder.InfrastructureCluster("", "").Build()
			err := fakeClient.Get(ctx, client.ObjectKeyFromObject(fakeInfraCluster), infraCluster)
			g.Expect(apierrors.IsNotFound(err)).To(Equal(tt.wantDelete))
		})
	}
}

func TestClusterReconcilerNodeRef(t *testing.T) {
	t.Run("machine to cluster", func(t *testing.T) {
		cluster := &clusterv1.Cluster{
			TypeMeta: metav1.TypeMeta{
				Kind: "Cluster",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-cluster",
				Namespace: "test",
			},
			Spec:   clusterv1.ClusterSpec{},
			Status: clusterv1.ClusterStatus{},
		}

		controlPlaneWithNoderef := &clusterv1.Machine{
			TypeMeta: metav1.TypeMeta{
				Kind: "Machine",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "controlPlaneWithNoderef",
				Namespace: "test",
				Labels: map[string]string{
					clusterv1.ClusterNameLabel:         cluster.Name,
					clusterv1.MachineControlPlaneLabel: "",
				},
			},
			Spec: clusterv1.MachineSpec{
				ClusterName: "test-cluster",
			},
			Status: clusterv1.MachineStatus{
				NodeRef: clusterv1.MachineNodeReference{Name: "test-node"},
			},
		}
		controlPlaneWithoutNoderef := &clusterv1.Machine{
			TypeMeta: metav1.TypeMeta{
				Kind: "Machine",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "controlPlaneWithoutNoderef",
				Namespace: "test",
				Labels: map[string]string{
					clusterv1.ClusterNameLabel:         cluster.Name,
					clusterv1.MachineControlPlaneLabel: "",
				},
			},
			Spec: clusterv1.MachineSpec{
				ClusterName: "test-cluster",
			},
		}
		nonControlPlaneWithNoderef := &clusterv1.Machine{
			TypeMeta: metav1.TypeMeta{
				Kind: "Machine",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "nonControlPlaneWitNoderef",
				Namespace: "test",
				Labels: map[string]string{
					clusterv1.ClusterNameLabel: cluster.Name,
				},
			},
			Spec: clusterv1.MachineSpec{
				ClusterName: "test-cluster",
			},
			Status: clusterv1.MachineStatus{
				NodeRef: clusterv1.MachineNodeReference{Name: "test-node"},
			},
		}
		nonControlPlaneWithoutNoderef := &clusterv1.Machine{
			TypeMeta: metav1.TypeMeta{
				Kind: "Machine",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "nonControlPlaneWithoutNoderef",
				Namespace: "test",
				Labels: map[string]string{
					clusterv1.ClusterNameLabel: cluster.Name,
				},
			},
			Spec: clusterv1.MachineSpec{
				ClusterName: "test-cluster",
			},
		}

		tests := []struct {
			name string
			o    client.Object
			want []ctrl.Request
		}{
			{
				name: "controlplane machine, noderef is set, should return cluster",
				o:    controlPlaneWithNoderef,
				want: []ctrl.Request{
					{
						NamespacedName: util.ObjectKey(cluster),
					},
				},
			},
			{
				name: "controlplane machine, noderef is not set",
				o:    controlPlaneWithoutNoderef,
				want: nil,
			},
			{
				name: "not controlplane machine, noderef is set",
				o:    nonControlPlaneWithNoderef,
				want: nil,
			},
			{
				name: "not controlplane machine, noderef is not set",
				o:    nonControlPlaneWithoutNoderef,
				want: nil,
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				g := NewWithT(t)

				c := fake.NewClientBuilder().WithObjects(cluster, controlPlaneWithNoderef, controlPlaneWithoutNoderef, nonControlPlaneWithNoderef, nonControlPlaneWithoutNoderef).Build()
				r := &Reconciler{
					Client: c,
				}
				requests := r.controlPlaneMachineToCluster(ctx, tt.o)
				g.Expect(requests).To(BeComparableTo(tt.want))
			})
		}
	})
}

type machineDeploymentBuilder struct {
	md clusterv1.MachineDeployment
}

func newMachineDeploymentBuilder() *machineDeploymentBuilder {
	return &machineDeploymentBuilder{}
}

func (b *machineDeploymentBuilder) named(name string) *machineDeploymentBuilder {
	b.md.Name = name
	return b
}

func (b *machineDeploymentBuilder) ownedBy(c *clusterv1.Cluster) *machineDeploymentBuilder {
	b.md.OwnerReferences = append(b.md.OwnerReferences, metav1.OwnerReference{
		APIVersion: clusterv1.GroupVersion.String(),
		Kind:       "Cluster",
		Name:       c.Name,
	})
	return b
}

func (b *machineDeploymentBuilder) build() clusterv1.MachineDeployment {
	return b.md
}

type machineSetBuilder struct {
	ms clusterv1.MachineSet
}

func newMachineSetBuilder() *machineSetBuilder {
	return &machineSetBuilder{}
}

func (b *machineSetBuilder) named(name string) *machineSetBuilder {
	b.ms.Name = name
	return b
}

func (b *machineSetBuilder) ownedBy(c *clusterv1.Cluster) *machineSetBuilder {
	b.ms.OwnerReferences = append(b.ms.OwnerReferences, metav1.OwnerReference{
		APIVersion: clusterv1.GroupVersion.String(),
		Kind:       "Cluster",
		Name:       c.Name,
	})
	return b
}

func (b *machineSetBuilder) build() clusterv1.MachineSet {
	return b.ms
}

type machineBuilder struct {
	m clusterv1.Machine
}

func newMachineBuilder() *machineBuilder {
	return &machineBuilder{}
}

func (b *machineBuilder) named(name string) *machineBuilder {
	b.m.Name = name
	return b
}

func (b *machineBuilder) ownedBy(c *clusterv1.Cluster) *machineBuilder {
	b.m.OwnerReferences = append(b.m.OwnerReferences, metav1.OwnerReference{
		APIVersion: clusterv1.GroupVersion.String(),
		Kind:       "Cluster",
		Name:       c.Name,
	})
	return b
}

func (b *machineBuilder) controlPlane() *machineBuilder {
	b.m.Labels = map[string]string{clusterv1.MachineControlPlaneLabel: ""}
	return b
}

func (b *machineBuilder) build() clusterv1.Machine {
	return b.m
}

type machinePoolBuilder struct {
	mp clusterv1.MachinePool
}

func newMachinePoolBuilder() *machinePoolBuilder {
	return &machinePoolBuilder{}
}

func (b *machinePoolBuilder) named(name string) *machinePoolBuilder {
	b.mp.Name = name
	return b
}

func (b *machinePoolBuilder) ownedBy(c *clusterv1.Cluster) *machinePoolBuilder {
	b.mp.OwnerReferences = append(b.mp.OwnerReferences, metav1.OwnerReference{
		APIVersion: clusterv1.GroupVersion.String(),
		Kind:       "Cluster",
		Name:       c.Name,
	})
	return b
}

func (b *machinePoolBuilder) build() clusterv1.MachinePool {
	return b.mp
}

func TestFilterOwnedDescendants(t *testing.T) {
	utilfeature.SetFeatureGateDuringTest(t, feature.Gates, feature.MachinePool, true)

	c := clusterv1.Cluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Cluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "c",
		},
	}

	md1NotOwnedByCluster := newMachineDeploymentBuilder().named("md1").build()
	md2OwnedByCluster := newMachineDeploymentBuilder().named("md2").ownedBy(&c).build()
	md3NotOwnedByCluster := newMachineDeploymentBuilder().named("md3").build()
	md4OwnedByCluster := newMachineDeploymentBuilder().named("md4").ownedBy(&c).build()

	ms1NotOwnedByCluster := newMachineSetBuilder().named("ms1").build()
	ms2OwnedByCluster := newMachineSetBuilder().named("ms2").ownedBy(&c).build()
	ms3NotOwnedByCluster := newMachineSetBuilder().named("ms3").build()
	ms4OwnedByCluster := newMachineSetBuilder().named("ms4").ownedBy(&c).build()

	m1NotOwnedByCluster := newMachineBuilder().named("m1").build()
	m2OwnedByCluster := newMachineBuilder().named("m2").ownedBy(&c).build()
	m3ControlPlaneOwnedByCluster := newMachineBuilder().named("m3").ownedBy(&c).controlPlane().build()
	m4NotOwnedByCluster := newMachineBuilder().named("m4").build()
	m5OwnedByCluster := newMachineBuilder().named("m5").ownedBy(&c).build()
	m6ControlPlaneOwnedByCluster := newMachineBuilder().named("m6").ownedBy(&c).controlPlane().build()

	mp1NotOwnedByCluster := newMachinePoolBuilder().named("mp1").build()
	mp2OwnedByCluster := newMachinePoolBuilder().named("mp2").ownedBy(&c).build()
	mp3NotOwnedByCluster := newMachinePoolBuilder().named("mp3").build()
	mp4OwnedByCluster := newMachinePoolBuilder().named("mp4").ownedBy(&c).build()

	d := clusterDescendants{
		machineDeployments: clusterv1.MachineDeploymentList{
			Items: []clusterv1.MachineDeployment{
				md1NotOwnedByCluster,
				md2OwnedByCluster,
				md3NotOwnedByCluster,
				md4OwnedByCluster,
			},
		},
		machineSets: clusterv1.MachineSetList{
			Items: []clusterv1.MachineSet{
				ms1NotOwnedByCluster,
				ms2OwnedByCluster,
				ms3NotOwnedByCluster,
				ms4OwnedByCluster,
			},
		},
		controlPlaneMachines: collections.FromMachineList(&clusterv1.MachineList{
			Items: []clusterv1.Machine{
				m3ControlPlaneOwnedByCluster,
				m6ControlPlaneOwnedByCluster,
			},
		}),
		workerMachines: collections.FromMachineList(&clusterv1.MachineList{
			Items: []clusterv1.Machine{
				m1NotOwnedByCluster,
				m2OwnedByCluster,
				m4NotOwnedByCluster,
				m5OwnedByCluster,
			},
		}),
		machinePools: clusterv1.MachinePoolList{
			Items: []clusterv1.MachinePool{
				mp1NotOwnedByCluster,
				mp2OwnedByCluster,
				mp3NotOwnedByCluster,
				mp4OwnedByCluster,
			},
		},
	}

	t.Run("Without a control plane object", func(t *testing.T) {
		g := NewWithT(t)

		actual, err := d.filterOwnedDescendants(&c)
		g.Expect(err).ToNot(HaveOccurred())

		g.Expect(actual).To(ConsistOf(
			&mp2OwnedByCluster,
			&mp4OwnedByCluster,
			&md2OwnedByCluster,
			&md4OwnedByCluster,
			&ms2OwnedByCluster,
			&ms4OwnedByCluster,
			&m2OwnedByCluster,
			&m5OwnedByCluster,
			&m3ControlPlaneOwnedByCluster,
			&m6ControlPlaneOwnedByCluster,
		))
	})

	t.Run("With a control plane object", func(t *testing.T) {
		g := NewWithT(t)

		cWithCP := c.DeepCopy()
		cWithCP.Spec.ControlPlaneRef = clusterv1.ContractVersionedObjectReference{
			Kind: "SomeKind",
		}

		actual, err := d.filterOwnedDescendants(cWithCP)
		g.Expect(err).ToNot(HaveOccurred())

		g.Expect(actual).To(ConsistOf(
			&mp2OwnedByCluster,
			&mp4OwnedByCluster,
			&md2OwnedByCluster,
			&md4OwnedByCluster,
			&ms2OwnedByCluster,
			&ms4OwnedByCluster,
			&m2OwnedByCluster,
			&m5OwnedByCluster,
		))
	})
}

func TestObjectsPendingDelete(t *testing.T) {
	// Note: Intentionally using random order to validate sorting.
	d := clusterDescendants{
		machineDeployments: clusterv1.MachineDeploymentList{
			Items: []clusterv1.MachineDeployment{
				newMachineDeploymentBuilder().named("md2").build(),
				newMachineDeploymentBuilder().named("md1").build(),
			},
		},
		machineSets: clusterv1.MachineSetList{
			Items: []clusterv1.MachineSet{
				newMachineSetBuilder().named("ms2").build(),
				newMachineSetBuilder().named("ms1").build(),
			},
		},
		controlPlaneMachines: collections.FromMachineList(&clusterv1.MachineList{
			Items: []clusterv1.Machine{
				newMachineBuilder().named("cp1").build(),
				newMachineBuilder().named("cp3").build(),
				newMachineBuilder().named("cp2").build(),
			},
		}),
		workerMachines: collections.FromMachineList(&clusterv1.MachineList{
			Items: []clusterv1.Machine{
				newMachineBuilder().named("w2").build(),
				newMachineBuilder().named("w1").build(),
				newMachineBuilder().named("w5").build(),
				newMachineBuilder().named("w6").build(),
				newMachineBuilder().named("w3").build(),
				newMachineBuilder().named("w4").build(),
				newMachineBuilder().named("w8").build(),
				newMachineBuilder().named("w7").build(),
			},
		}),
		machinePools: clusterv1.MachinePoolList{
			Items: []clusterv1.MachinePool{
				newMachinePoolBuilder().named("mp2").build(),
				newMachinePoolBuilder().named("mp1").build(),
			},
		},
	}

	t.Run("Without a control plane object", func(t *testing.T) {
		g := NewWithT(t)

		c := &clusterv1.Cluster{}
		g.Expect(d.objectsPendingDeleteCount(c)).To(Equal(17))
		g.Expect(d.objectsPendingDeleteNames(c)).To(Equal([]string{"Control plane Machines: cp1, cp2, cp3", "MachineDeployments: md1, md2", "MachineSets: ms1, ms2", "MachinePools: mp1, mp2", "Worker Machines: w1, w2, w3, w4, w5, ... (3 more)"}))
	})

	t.Run("With a control plane object", func(t *testing.T) {
		g := NewWithT(t)

		c := &clusterv1.Cluster{Spec: clusterv1.ClusterSpec{ControlPlaneRef: clusterv1.ContractVersionedObjectReference{Kind: "SomeKind"}}}
		g.Expect(d.objectsPendingDeleteCount(c)).To(Equal(14))
		g.Expect(d.objectsPendingDeleteNames(c)).To(Equal([]string{"MachineDeployments: md1, md2", "MachineSets: ms1, ms2", "MachinePools: mp1, mp2", "Worker Machines: w1, w2, w3, w4, w5, ... (3 more)"}))
	})
}

func TestReconcileV1Beta1ControlPlaneInitializedControlPlaneRef(t *testing.T) {
	g := NewWithT(t)

	c := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "c",
		},
		Spec: clusterv1.ClusterSpec{
			ControlPlaneRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: "test.io",
				Name:     "foo",
			},
		},
	}

	r := &Reconciler{}

	s := &scope{
		cluster: c,
	}
	res, err := r.reconcileV1Beta1ControlPlaneInitialized(ctx, s)
	g.Expect(res.IsZero()).To(BeTrue())
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(v1beta1conditions.Has(c, clusterv1.ControlPlaneInitializedV1Beta1Condition)).To(BeFalse())
}
