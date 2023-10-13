/*
Copyright 2022 The Kubernetes Authors.

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

package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/syself/cluster-api-provider-hetzner/api/v1beta1"
	"github.com/syself/cluster-api-provider-hetzner/pkg/utils"
	"github.com/syself/cluster-api-provider-hetzner/test/helpers"
)

var _ = Describe("Hetzner ClusterReconciler", func() {
	Context("cluster tests", func() {
		var (
			err       error
			namespace string
			testNs    *corev1.Namespace

			instance    *infrav1.HetznerCluster
			capiCluster *clusterv1.Cluster

			hetznerSecret *corev1.Secret

			key                client.ObjectKey
			lbName             string
			hetznerClusterName string
		)
		BeforeEach(func() {
			testNs, err = testEnv.CreateNamespace(ctx, "cluster-tests")
			Expect(err).NotTo(HaveOccurred())
			namespace = testNs.Name

			lbName = utils.GenerateName(nil, "myloadbalancer")

			hetznerClusterName = utils.GenerateName(nil, "hetzner-test1")
			// Create capi cluster
			capiCluster = &clusterv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test1-",
					Namespace:    namespace,
					Finalizers:   []string{clusterv1.ClusterFinalizer},
				},
				Spec: clusterv1.ClusterSpec{
					InfrastructureRef: &corev1.ObjectReference{
						APIVersion: infrav1.GroupVersion.String(),
						Kind:       "HetznerCluster",
						Name:       hetznerClusterName,
					},
				},
			}
			Expect(testEnv.Create(ctx, capiCluster)).To(Succeed())

			// Create the HetznerCluster object
			instance = &infrav1.HetznerCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      hetznerClusterName,
					Namespace: namespace,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "cluster.x-k8s.io/v1beta1",
							Kind:       "Cluster",
							Name:       capiCluster.Name,
							UID:        capiCluster.UID,
						},
					},
				},
				Spec: getDefaultHetznerClusterSpec(),
			}

			hetznerSecret = getDefaultHetznerSecret(namespace)
			Expect(testEnv.Create(ctx, hetznerSecret)).To(Succeed())

			key = client.ObjectKey{Namespace: namespace, Name: hetznerClusterName}
		})

		AfterEach(func() {
			Expect(testEnv.Cleanup(ctx, testNs, capiCluster, instance, hetznerSecret)).To(Succeed())
		})

		It("should set the finalizer", func() {
			Expect(testEnv.Create(ctx, instance)).To(Succeed())

			Eventually(func() bool {
				if err := testEnv.Get(ctx, key, instance); err != nil {
					return false
				}
				return len(instance.Finalizers) > 0
			}, timeout, time.Second).Should(BeTrue())
		})

		Context("load balancer", func() {
			It("should create load balancer and update it accordingly", func() {
				Expect(testEnv.Create(ctx, instance)).To(Succeed())

				Eventually(func() bool {
					return isPresentAndTrue(key, instance, infrav1.LoadBalancerReadyCondition)
				}, timeout, time.Second).Should(BeTrue())

				newLBName := "new-lb-name"
				newLBType := "lb31"

				By("updating load balancer type")

				ph, err := patch.NewHelper(instance, testEnv)
				Expect(err).ShouldNot(HaveOccurred())

				instance.Spec.ControlPlaneLoadBalancer.Type = newLBType

				Eventually(func() error {
					return ph.Patch(ctx, instance, patch.WithStatusObservedGeneration{})
				}, timeout).Should(BeNil())

				By("updating load balancer name")

				ph, err = patch.NewHelper(instance, testEnv)
				Expect(err).ShouldNot(HaveOccurred())

				instance.Spec.ControlPlaneLoadBalancer.Name = &newLBName

				Eventually(func() error {
					return ph.Patch(ctx, instance, patch.WithStatusObservedGeneration{})
				}, timeout).Should(BeNil())

				By("listing load balancers and checking spec")

				// Check in hetzner API
				Eventually(func() bool {
					loadBalancers, err := hcloudClient.ListLoadBalancers(ctx, hcloud.LoadBalancerListOpts{
						ListOpts: hcloud.ListOpts{
							LabelSelector: utils.LabelsToLabelSelector(map[string]string{instance.ClusterTagKey(): "owned"}),
						},
					})
					if err != nil {
						testEnv.GetLogger().Info("failed to list load balancers", "err", err)
						return false
					}
					if len(loadBalancers) > 1 {
						testEnv.GetLogger().Info("there are multiple load balancers found", "number of load balancers", loadBalancers)
						return false
					}
					if len(loadBalancers) == 0 {
						testEnv.GetLogger().Info("no load balancer found")
						return false
					}

					lb := loadBalancers[0]

					if lb.Name != newLBName {
						testEnv.GetLogger().Info("wrong name", "want", newLBName, "got", lb.Name)
						return false
					}
					if lb.LoadBalancerType.Name != newLBType {
						testEnv.GetLogger().Info("wrong type", "want", newLBType, "got", lb.LoadBalancerType.Name)
						return false
					}

					return true
				}, timeout, 1*time.Second).Should(BeTrue())
			})

			It("should update extra targets", func() {
				Expect(testEnv.Create(ctx, instance)).To(Succeed())

				Eventually(func() bool {
					return isPresentAndTrue(key, instance, infrav1.LoadBalancerReadyCondition)
				}, timeout).Should(BeTrue())

				By("adding additional extra services")

				ph, err := patch.NewHelper(instance, testEnv)
				Expect(err).ShouldNot(HaveOccurred())

				instance.Spec.ControlPlaneLoadBalancer.ExtraServices = append(instance.Spec.ControlPlaneLoadBalancer.ExtraServices,
					infrav1.LoadBalancerServiceSpec{
						DestinationPort: 8134,
						ListenPort:      8134,
						Protocol:        "tcp",
					})

				Eventually(func() error {
					return ph.Patch(ctx, instance, patch.WithStatusObservedGeneration{})
				}, timeout).Should(BeNil())

				Eventually(func() int {
					loadBalancers, err := hcloudClient.ListLoadBalancers(ctx, hcloud.LoadBalancerListOpts{
						ListOpts: hcloud.ListOpts{
							LabelSelector: utils.LabelsToLabelSelector(map[string]string{instance.ClusterTagKey(): "owned"}),
						},
					})
					if err != nil {
						return -1
					}
					if len(loadBalancers) > 1 {
						return -2
					}
					if len(loadBalancers) == 0 {
						return -3
					}
					lb := loadBalancers[0]

					return len(lb.Services)
				}, timeout).Should(Equal(len(instance.Spec.ControlPlaneLoadBalancer.ExtraServices) + 1))

				By("reducing extra targets")

				ph, err = patch.NewHelper(instance, testEnv)
				Expect(err).ShouldNot(HaveOccurred())
				instance.Spec.ControlPlaneLoadBalancer.ExtraServices = []infrav1.LoadBalancerServiceSpec{
					{
						DestinationPort: 8134,
						ListenPort:      8134,
						Protocol:        "tcp",
					},
				}

				Eventually(func() error {
					return ph.Patch(ctx, instance, patch.WithStatusObservedGeneration{})
				}, timeout).Should(BeNil())

				Eventually(func() int {
					loadBalancers, err := hcloudClient.ListLoadBalancers(ctx, hcloud.LoadBalancerListOpts{
						ListOpts: hcloud.ListOpts{
							LabelSelector: utils.LabelsToLabelSelector(map[string]string{instance.ClusterTagKey(): "owned"}),
						},
					})
					if err != nil {
						return -1
					}
					if len(loadBalancers) > 1 {
						return -2
					}
					if len(loadBalancers) == 0 {
						return -3
					}
					lb := loadBalancers[0]

					return len(lb.Services)
				}, timeout).Should(Equal(len(instance.Spec.ControlPlaneLoadBalancer.ExtraServices) + 1))

				By("removing extra targets")

				ph, err = patch.NewHelper(instance, testEnv)
				Expect(err).ShouldNot(HaveOccurred())
				instance.Spec.ControlPlaneLoadBalancer.ExtraServices = nil

				Eventually(func() error {
					return ph.Patch(ctx, instance, patch.WithStatusObservedGeneration{})
				}, timeout).Should(BeNil())

				Eventually(func() int {
					loadBalancers, err := hcloudClient.ListLoadBalancers(ctx, hcloud.LoadBalancerListOpts{
						ListOpts: hcloud.ListOpts{
							LabelSelector: utils.LabelsToLabelSelector(map[string]string{instance.ClusterTagKey(): "owned"}),
						},
					})
					if err != nil {
						return -1
					}
					if len(loadBalancers) > 1 {
						return -2
					}
					if len(loadBalancers) == 0 {
						return -3
					}
					lb := loadBalancers[0]

					return len(lb.Services)
				}, timeout).Should(Equal(len(instance.Spec.ControlPlaneLoadBalancer.ExtraServices) + 1))
			})

			It("should not create load balancer if disabled and the cluster should get ready", func() {
				instance.Spec.ControlPlaneLoadBalancer.Enabled = false
				instance.Spec.ControlPlaneEndpoint = &clusterv1.APIEndpoint{
					Host: "my.test.host",
					Port: 6443,
				}
				Expect(testEnv.Create(ctx, instance)).To(Succeed())

				Eventually(func() bool {
					if err := testEnv.Get(ctx, key, instance); err != nil {
						return false
					}

					return instance.Status.ControlPlaneLoadBalancer == nil && instance.Status.Ready
				}, timeout, time.Second).Should(BeTrue())
			})

			It("should take over an existing load balancer with correct name", func() {
				By("creating load balancer manually")

				opts := hcloud.LoadBalancerCreateOpts{
					Name:             lbName,
					Algorithm:        &hcloud.LoadBalancerAlgorithm{Type: hcloud.LoadBalancerAlgorithmTypeLeastConnections},
					LoadBalancerType: &hcloud.LoadBalancerType{Name: "mytype"},
				}

				_, err := hcloudClient.CreateLoadBalancer(ctx, opts)
				Expect(err).To(BeNil())

				By("making sure that there is no label set")
				loadBalancers, err := hcloudClient.ListLoadBalancers(ctx, hcloud.LoadBalancerListOpts{Name: lbName})
				Expect(err).To(BeNil())
				Expect(loadBalancers).To(HaveLen(1))

				_, found := loadBalancers[0].Labels[instance.ClusterTagKey()]
				Expect(found).To(BeFalse())

				By("creating cluster object")

				instance.Spec.ControlPlaneLoadBalancer.Name = &lbName
				Expect(testEnv.Create(ctx, instance)).To(Succeed())

				By("checking that cluster is ready")

				Eventually(func() bool {
					return isPresentAndTrue(key, instance, infrav1.LoadBalancerReadyCondition)
				}, timeout, time.Second).Should(BeTrue())

				By("checking that load balancer has label set")

				loadBalancers, err = hcloudClient.ListLoadBalancers(ctx, hcloud.LoadBalancerListOpts{Name: lbName})
				Expect(err).To(BeNil())
				Expect(loadBalancers).To(HaveLen(1))

				value, found := loadBalancers[0].Labels[instance.ClusterTagKey()]
				Expect(found).To(BeTrue())
				Expect(value).To(Equal(string(infrav1.ResourceLifecycleOwned)))

				By("checking that kubeapi service is set on load balancer")

				var foundHetznerCluster infrav1.HetznerCluster

				Eventually(func() bool {
					if err := testEnv.Get(ctx, key, &foundHetznerCluster); err != nil {
						testEnv.GetLogger().Error(err, "failed to fetch HetznerCluster")
						return false
					}

					// fetch load balancer again as reconcilement of additional services happens after the load balancer has been created
					loadBalancers, err := hcloudClient.ListLoadBalancers(ctx, hcloud.LoadBalancerListOpts{Name: lbName})
					if err != nil {
						testEnv.GetLogger().Error(err, "failed to list load balancers")
						return false
					}

					if len(loadBalancers) != 1 {
						testEnv.GetLogger().Info("expect 1 load balancer - but did not get it", "got", len(loadBalancers))
						return false
					}

					lb := loadBalancers[0]
					for _, service := range lb.Services {
						if service.ListenPort == int(foundHetznerCluster.Spec.ControlPlaneEndpoint.Port) {
							return true
						}
					}

					testEnv.GetLogger().Info(
						"Could not find listenPort of kubeapiserver in load balancer services",
						"load balancer services", lb.Services,
						"listenPort of kubeAPI service", foundHetznerCluster.Spec.ControlPlaneEndpoint.Port,
					)
					return false
				}, timeout, time.Second).Should(BeTrue())

				By("deleting the cluster and load balancer and testing that owned label is gone")

				Expect(testEnv.Delete(ctx, instance))

				Eventually(func() bool {
					loadBalancers, err := hcloudClient.ListLoadBalancers(ctx, hcloud.LoadBalancerListOpts{Name: lbName})
					// there should always be one load balancer, if not, then this is a problem where we can immediately return
					Expect(err).To(BeNil())
					Expect(loadBalancers).To(HaveLen(1))

					_, found := loadBalancers[0].Labels[instance.ClusterTagKey()]
					return found
				}, timeout, time.Second).Should(BeFalse())
			})

			It("should set the appropriate condition if a named load balancer is taken by another cluster", func() {
				By("creating load balancer manually")
				labelsOwnedByOtherCluster := map[string]string{instance.ClusterTagKey() + "s": string(infrav1.ResourceLifecycleOwned)}
				opts := hcloud.LoadBalancerCreateOpts{
					Name:             lbName,
					Algorithm:        &hcloud.LoadBalancerAlgorithm{Type: hcloud.LoadBalancerAlgorithmTypeLeastConnections},
					LoadBalancerType: &hcloud.LoadBalancerType{Name: "mytype"},
					Labels:           labelsOwnedByOtherCluster,
				}

				_, err := hcloudClient.CreateLoadBalancer(ctx, opts)
				Expect(err).To(BeNil())

				By("creating cluster object")

				instance.Spec.ControlPlaneLoadBalancer.Name = &lbName
				Expect(testEnv.Create(ctx, instance)).To(Succeed())

				By("checking that cluster is ready")

				Eventually(func() bool {
					return isPresentAndFalseWithReason(key, instance, infrav1.LoadBalancerReadyCondition, infrav1.LoadBalancerFailedToOwnReason)
				}, timeout, time.Second).Should(BeTrue())
			})

			It("should set the appropriate condition if a named load balancer is not found", func() {
				By("creating cluster object")

				instance.Spec.ControlPlaneLoadBalancer.Name = &lbName
				Expect(testEnv.Create(ctx, instance)).To(Succeed())

				By("checking that cluster has condition set")

				Eventually(func() bool {
					return isPresentAndFalseWithReason(key, instance, infrav1.LoadBalancerReadyCondition, infrav1.LoadBalancerFailedToOwnReason)
				}, timeout, time.Second).Should(BeTrue())
			})
		})

		Context("HetznerMachines belonging to the cluster", func() {
			var bootstrapSecret *corev1.Secret

			BeforeEach(func() {
				bootstrapSecret = getDefaultBootstrapSecret(namespace)
				Expect(testEnv.Create(ctx, bootstrapSecret)).To(Succeed())
			})

			AfterEach(func() {
				Expect(testEnv.Cleanup(ctx, bootstrapSecret)).To(Succeed())
			})

			It("sets owner references to those machines", func() {
				Expect(testEnv.Create(ctx, instance)).To(Succeed())

				By("creating hcloudmachine objects")

				machineCount := 3
				for i := 0; i < machineCount; i++ {
					Expect(createCapiAndHcloudMachines(ctx, testEnv, namespace, capiCluster.Name)).To(Succeed())
				}

				By("checking labels of HCloudMachine objects")

				Eventually(func() int {
					servers, err := hcloudClient.ListServers(ctx, hcloud.ServerListOpts{
						ListOpts: hcloud.ListOpts{
							LabelSelector: utils.LabelsToLabelSelector(map[string]string{instance.ClusterTagKey(): "owned"}),
						},
					})
					if err != nil {
						return -1
					}
					return len(servers)
				}, timeout).Should(Equal(machineCount))
			})
		})

		Context("Placement groups", func() {
			var bootstrapSecret *corev1.Secret

			BeforeEach(func() {
				// Create the bootstrap secret
				bootstrapSecret = getDefaultBootstrapSecret(namespace)
				Expect(testEnv.Create(ctx, bootstrapSecret)).To(Succeed())
			})

			AfterEach(func() {
				Expect(testEnv.Cleanup(ctx, bootstrapSecret)).To(Succeed())
			})

			DescribeTable("create and delete placement groups without error",
				func(placementGroups []infrav1.HCloudPlacementGroupSpec) {
					instance.Spec.HCloudPlacementGroups = placementGroups
					Expect(testEnv.Create(ctx, instance)).To(Succeed())

					Eventually(func() bool {
						return isPresentAndTrue(key, instance, infrav1.PlacementGroupsSyncedCondition)
					}, timeout).Should(BeTrue())

					By("checking for presence of HCloudPlacementGroup objects")

					Eventually(func() int {
						pgs, err := hcloudClient.ListPlacementGroups(ctx, hcloud.PlacementGroupListOpts{
							ListOpts: hcloud.ListOpts{
								LabelSelector: utils.LabelsToLabelSelector(map[string]string{instance.ClusterTagKey(): "owned"}),
							},
						})
						if err != nil {
							return -1
						}
						return len(pgs)
					}, timeout).Should(Equal(len(placementGroups)))
				},
				Entry("placement groups", []infrav1.HCloudPlacementGroupSpec{
					{
						Name: defaultPlacementGroupName,
						Type: "spread",
					},
					{
						Name: "md-0",
						Type: "spread",
					},
				}),
				Entry("no placement groups", []infrav1.HCloudPlacementGroupSpec{}),
			)

			Context("update placement groups", func() {
				BeforeEach(func() {
					Expect(testEnv.Create(ctx, instance)).To(Succeed())
				})

				DescribeTable("update placement groups",
					func(newPlacementGroupSpec []infrav1.HCloudPlacementGroupSpec) {
						ph, err := patch.NewHelper(instance, testEnv)
						Expect(err).ShouldNot(HaveOccurred())

						instance.Spec.HCloudPlacementGroups = newPlacementGroupSpec

						Eventually(func() error {
							return ph.Patch(ctx, instance, patch.WithStatusObservedGeneration{})
						}, timeout).Should(BeNil())

						Eventually(func() int {
							pgs, err := hcloudClient.ListPlacementGroups(ctx, hcloud.PlacementGroupListOpts{
								ListOpts: hcloud.ListOpts{
									LabelSelector: utils.LabelsToLabelSelector(map[string]string{instance.ClusterTagKey(): "owned"}),
								},
							})
							if err != nil {
								return -1
							}
							return len(pgs)
						}, timeout, time.Second).Should(Equal(len(newPlacementGroupSpec)))
					},
					Entry("one pg", []infrav1.HCloudPlacementGroupSpec{{Name: "md-0", Type: "spread"}}),
					Entry("no pgs", []infrav1.HCloudPlacementGroupSpec{}),
					Entry("three pgs", []infrav1.HCloudPlacementGroupSpec{
						{Name: "md-0", Type: "spread"},
						{Name: "md-1", Type: "spread"},
						{Name: "md-2", Type: "spread"},
					}),
				)
			})
		})

		Context("network", func() {
			var bootstrapSecret *corev1.Secret

			BeforeEach(func() {
				bootstrapSecret = getDefaultBootstrapSecret(namespace)
				Expect(testEnv.Create(ctx, bootstrapSecret)).To(Succeed())
			})

			AfterEach(func() {
				Expect(testEnv.Delete(ctx, bootstrapSecret)).To(Succeed())
			})

			It("creates a cluster with network and gets ready", func() {
				Expect(testEnv.Create(ctx, instance)).To(Succeed())

				Eventually(func() bool {
					return isPresentAndTrue(key, instance, infrav1.NetworkReadyCondition)
				}, timeout).Should(BeTrue())
			},
			)
		})
	})
})

func createCapiAndHcloudMachines(ctx context.Context, env *helpers.TestEnvironment, namespace, clusterName string) error {
	hcloudMachineName := utils.GenerateName(nil, "hcloud-machine")
	capiMachine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "capi-machine-",
			Namespace:    namespace,
			Finalizers:   []string{clusterv1.MachineFinalizer},
			Labels: map[string]string{
				clusterv1.ClusterNameLabel: clusterName,
			},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName: clusterName,
			InfrastructureRef: corev1.ObjectReference{
				APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
				Kind:       "HCloudMachine",
				Name:       hcloudMachineName,
			},
			FailureDomain: &defaultFailureDomain,
			Bootstrap: clusterv1.Bootstrap{
				DataSecretName: ptr.To("bootstrap-secret"),
			},
		},
	}
	if err := env.Create(ctx, capiMachine); err != nil {
		return err
	}

	hcloudMachine := &infrav1.HCloudMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      hcloudMachineName,
			Namespace: namespace,
			Labels:    map[string]string{clusterv1.ClusterNameLabel: clusterName},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: clusterv1.GroupVersion.String(),
					Kind:       "Machine",
					Name:       capiMachine.Name,
					UID:        capiMachine.UID,
				},
			},
		},
		Spec: infrav1.HCloudMachineSpec{
			ImageName: "fedora-control-plane",
			Type:      "cpx31",
		},
	}
	return env.Create(ctx, hcloudMachine)
}

var _ = Describe("Hetzner secret", func() {
	var (
		testNs         *corev1.Namespace
		hetznerCluster *infrav1.HetznerCluster
		capiCluster    *clusterv1.Cluster

		hetznerSecret *corev1.Secret

		key                client.ObjectKey
		hetznerClusterName string
	)

	BeforeEach(func() {
		var err error
		testNs, err = testEnv.CreateNamespace(ctx, "hetzner-secret")
		Expect(err).NotTo(HaveOccurred())

		hetznerClusterName = utils.GenerateName(nil, "hetzner-cluster-test")
		capiCluster = &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test1-",
				Namespace:    testNs.Name,
				Finalizers:   []string{clusterv1.ClusterFinalizer},
			},
			Spec: clusterv1.ClusterSpec{
				InfrastructureRef: &corev1.ObjectReference{
					APIVersion: infrav1.GroupVersion.String(),
					Kind:       "HetznerCluster",
					Name:       hetznerClusterName,
					Namespace:  testNs.Name,
				},
			},
		}
		Expect(testEnv.Create(ctx, capiCluster)).To(Succeed())

		hetznerCluster = &infrav1.HetznerCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      hetznerClusterName,
				Namespace: testNs.Name,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "cluster.x-k8s.io/v1beta1",
						Kind:       "Cluster",
						Name:       capiCluster.Name,
						UID:        capiCluster.UID,
					},
				},
			},
			Spec: getDefaultHetznerClusterSpec(),
		}
		Expect(testEnv.Create(ctx, hetznerCluster)).To(Succeed())

		key = client.ObjectKey{Namespace: hetznerCluster.Namespace, Name: hetznerCluster.Name}
	})

	AfterEach(func() {
		Expect(testEnv.Cleanup(ctx, hetznerCluster, capiCluster, hetznerSecret)).To(Succeed())
	})

	DescribeTable("test different hetzner secret",
		func(secretFunc func() *corev1.Secret, expectedReason string) {
			hetznerSecret = secretFunc()
			Expect(testEnv.Create(ctx, hetznerSecret)).To(Succeed())

			Eventually(func() bool {
				return isPresentAndFalseWithReason(key, hetznerCluster, infrav1.HCloudTokenAvailableCondition, expectedReason)
			}, timeout, time.Second).Should(BeTrue())
		},
		Entry("no Hetzner secret/wrong reference", func() *corev1.Secret {
			return &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "wrong-name",
					Namespace: testNs.Name,
				},
				Data: map[string][]byte{
					"hcloud": []byte("my-token"),
				},
			}
		}, infrav1.HetznerSecretUnreachableReason),
		Entry("empty hcloud token", func() *corev1.Secret {
			return &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hetzner-secret",
					Namespace: testNs.Name,
				},
				Data: map[string][]byte{
					"hcloud": []byte(""),
				},
			}
		}, infrav1.HCloudCredentialsInvalidReason),
		Entry("wrong key in secret", func() *corev1.Secret {
			return &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hetzner-secret",
					Namespace: testNs.Name,
				},
				Data: map[string][]byte{
					"wrongkey": []byte("my-token"),
				},
			}
		}, infrav1.HCloudCredentialsInvalidReason),
	)
})

var _ = Describe("HetznerCluster validation", func() {
	var (
		hetznerCluster *infrav1.HetznerCluster
		testNs         *corev1.Namespace
	)
	BeforeEach(func() {
		var err error
		testNs, err = testEnv.CreateNamespace(ctx, "hcloudmachine-validation")
		Expect(err).NotTo(HaveOccurred())
	})
	AfterEach(func() {
		Expect(testEnv.Cleanup(ctx, testNs, hetznerCluster)).To(Succeed())
	})

	Context("validate create", func() {
		BeforeEach(func() {
			hetznerCluster = &infrav1.HetznerCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hcloud-validation-machine",
					Namespace: testNs.Name,
				},
				Spec: getDefaultHetznerClusterSpec(),
			}
		})

		It("should succeed with valid spec", func() {
			Expect(testEnv.Create(ctx, hetznerCluster)).To(Succeed())
		})

		It("should fail without a wrong controlPlaneRegion name", func() {
			hetznerCluster.Spec.ControlPlaneRegions = append(hetznerCluster.Spec.ControlPlaneRegions, infrav1.Region("wrong-region"))
			Expect(testEnv.Create(ctx, hetznerCluster)).ToNot(Succeed())
		})

		It("should fail with an SSHKey without name", func() {
			hetznerCluster.Spec.SSHKeys.HCloud = append(hetznerCluster.Spec.SSHKeys.HCloud, infrav1.SSHKey{})
			Expect(testEnv.Create(ctx, hetznerCluster)).ToNot(Succeed())
		})

		It("should fail with an empty controlPlaneLoadBalancer region", func() {
			hetznerCluster.Spec.ControlPlaneLoadBalancer.Region = ""
			Expect(testEnv.Create(ctx, hetznerCluster)).ToNot(Succeed())
		})

		It("should fail with an empty placementGroup name", func() {
			hetznerCluster.Spec.HCloudPlacementGroups = append(hetznerCluster.Spec.HCloudPlacementGroups, infrav1.HCloudPlacementGroupSpec{})
			Expect(testEnv.Create(ctx, hetznerCluster)).ToNot(Succeed())
		})

		It("should fail with a wrong placementGroup type", func() {
			hetznerCluster.Spec.HCloudPlacementGroups = append(hetznerCluster.Spec.HCloudPlacementGroups, infrav1.HCloudPlacementGroupSpec{
				Name: "newName",
				Type: "wrong-type",
			})
			Expect(testEnv.Create(ctx, hetznerCluster)).ToNot(Succeed())
		})
	})
})

var _ = Describe("reconcileRateLimit", func() {
	var hetznerCluster *infrav1.HetznerCluster
	BeforeEach(func() {
		hetznerCluster = &infrav1.HetznerCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "rate-limit-cluster",
				Namespace: "default",
			},
			Spec: getDefaultHetznerClusterSpec(),
		}
	})

	It("returns wait==true if rate limit exceeded is set and time is not over", func() {
		conditions.MarkFalse(hetznerCluster, infrav1.HetznerAPIReachableCondition, infrav1.RateLimitExceededReason, clusterv1.ConditionSeverityWarning, "")
		Expect(reconcileRateLimit(hetznerCluster, testEnv.RateLimitWaitTime)).To(BeTrue())
	})

	It("returns wait==false if rate limit exceeded is set and time is over", func() {
		conditions.MarkFalse(hetznerCluster, infrav1.HetznerAPIReachableCondition, infrav1.RateLimitExceededReason, clusterv1.ConditionSeverityWarning, "")
		conditionList := hetznerCluster.GetConditions()
		conditionList[0].LastTransitionTime = metav1.NewTime(time.Now().Add(-time.Hour))
		Expect(reconcileRateLimit(hetznerCluster, testEnv.RateLimitWaitTime)).To(BeFalse())
	})

	It("returns wait==false if rate limit condition is set to true", func() {
		conditions.MarkTrue(hetznerCluster, infrav1.HetznerAPIReachableCondition)
		Expect(reconcileRateLimit(hetznerCluster, testEnv.RateLimitWaitTime)).To(BeFalse())
	})

	It("returns wait==false if rate limit condition is not set", func() {
		Expect(reconcileRateLimit(hetznerCluster, testEnv.RateLimitWaitTime)).To(BeFalse())
	})
})

func TestSetControlPlaneEndpoint(t *testing.T) {
	tests := []struct{
		name string
		hetznerCluster *infrav1.HetznerCluster
		want bool
	}{
		{
			name: "the function should return true if Loadbalancer is enabled, IPV4 does not include a <nil> string and the ControlPlaneEndpoint is set to nil",
			hetznerCluster: &infrav1.HetznerCluster{Spec: infrav1.HetznerClusterSpec{ControlPlaneLoadBalancer: infrav1.LoadBalancerSpec{Enabled: true}, ControlPlaneEndpoint: nil}, Status: infrav1.HetznerClusterStatus{ControlPlaneLoadBalancer: &infrav1.LoadBalancerStatus{IPv4: "xyz"}}},
			want: true,
		},
		{
			name: "the function should return true if Loadbalancer is enabled, IPV4 does not include a <nil> string and the ControlPlaneEndpoint is NOT set to nil",
			hetznerCluster: &infrav1.HetznerCluster{Spec: infrav1.HetznerClusterSpec{ControlPlaneLoadBalancer: infrav1.LoadBalancerSpec{Enabled: true}, ControlPlaneEndpoint: &clusterv1.APIEndpoint{Host: "", Port: 0}}, Status: infrav1.HetznerClusterStatus{ControlPlaneLoadBalancer: &infrav1.LoadBalancerStatus{IPv4: "xyz"}}},
			want: true,
		},
		{
			name: "the function should return true if Loadbalancer is enabled, IPV4 does not include a <nil> string and the ControlPlaneEndpoint contains preset values in its Host and Port component",
			hetznerCluster: &infrav1.HetznerCluster{Spec: infrav1.HetznerClusterSpec{ControlPlaneLoadBalancer: infrav1.LoadBalancerSpec{Enabled: true}, ControlPlaneEndpoint: &clusterv1.APIEndpoint{Host: "xyz", Port: 1}}, Status: infrav1.HetznerClusterStatus{ControlPlaneLoadBalancer: &infrav1.LoadBalancerStatus{IPv4: "xyz"}}},
			want: true,
		},
		{
			name: "the function should return false if Loadbalancer is not enabled and the ControlPlaneEndpoint is nil",
			hetznerCluster: &infrav1.HetznerCluster{Spec: infrav1.HetznerClusterSpec{ControlPlaneLoadBalancer: infrav1.LoadBalancerSpec{Enabled: false}, ControlPlaneEndpoint: nil}},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T){
			got := SetControlPlaneEndpoint(tt.hetznerCluster)
			assert.Equal(t, tt.want, got)
		})
	}
}