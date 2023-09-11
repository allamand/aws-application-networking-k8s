package test

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/onsi/gomega/format"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/external-dns/endpoint"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"

	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/vpclattice"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	"github.com/samber/lo/parallel"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gateway_api_v1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
	gateway_api_v1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
	mcs_api "sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"

	"github.com/aws/aws-application-networking-k8s/controllers"
	"github.com/aws/aws-application-networking-k8s/pkg/apis/applicationnetworking/v1alpha1"
	"github.com/aws/aws-application-networking-k8s/pkg/config"
	"github.com/aws/aws-application-networking-k8s/pkg/model/core"
	"github.com/aws/aws-application-networking-k8s/pkg/model/lattice"
	"github.com/aws/aws-application-networking-k8s/pkg/utils"
	"github.com/aws/aws-application-networking-k8s/pkg/utils/gwlog"

	"github.com/aws/aws-application-networking-k8s/pkg/aws/services"
	"github.com/aws/aws-application-networking-k8s/pkg/latticestore"
)

type TestObject struct {
	Type     client.Object
	ListType client.ObjectList
}

var (
	testScheme          = runtime.NewScheme()
	CurrentClusterVpcId = os.Getenv("CLUSTER_VPC_ID")
	TestObjects         = []TestObject{
		{&gateway_api_v1beta1.HTTPRoute{}, &gateway_api_v1beta1.HTTPRouteList{}},
		{&mcs_api.ServiceExport{}, &mcs_api.ServiceExportList{}},
		{&mcs_api.ServiceImport{}, &mcs_api.ServiceImportList{}},
		{&gateway_api_v1beta1.Gateway{}, &gateway_api_v1beta1.GatewayList{}},
		{&appsv1.Deployment{}, &appsv1.DeploymentList{}},
		{&v1.Service{}, &v1.ServiceList{}},
	}
)

func init() {
	format.MaxLength = 0
	utilruntime.Must(clientgoscheme.AddToScheme(testScheme))
	utilruntime.Must(gateway_api_v1alpha2.AddToScheme(testScheme))
	utilruntime.Must(gateway_api_v1beta1.AddToScheme(testScheme))
	utilruntime.Must(mcs_api.AddToScheme(testScheme))
	addOptionalCRDs(testScheme)
}

func addOptionalCRDs(scheme *runtime.Scheme) {
	dnsEndpoint := schema.GroupVersion{
		Group:   "externaldns.k8s.io",
		Version: "v1alpha1",
	}
	scheme.AddKnownTypes(dnsEndpoint, &endpoint.DNSEndpoint{}, &endpoint.DNSEndpointList{})
	metav1.AddToGroupVersion(scheme, dnsEndpoint)

	targetGroupPolicy := schema.GroupVersion{
		Group:   "application-networking.k8s.aws",
		Version: "v1alpha1",
	}
	scheme.AddKnownTypes(targetGroupPolicy, &v1alpha1.TargetGroupPolicy{}, &v1alpha1.TargetGroupPolicyList{})
	metav1.AddToGroupVersion(scheme, targetGroupPolicy)
}

type Framework struct {
	client.Client
	ctx                                 context.Context
	log                                 gwlog.Logger
	k8sScheme                           *runtime.Scheme
	namespace                           string
	controllerRuntimeConfig             *rest.Config
	LatticeClient                       services.Lattice
	GrpcurlRunner                       *v1.Pod
	TestCasesCreatedServiceNetworkNames map[string]bool //key: ServiceNetworkName; value: not in use, meaningless
	TestCasesCreatedServiceNames        map[string]bool //key: ServiceName; value not in use, meaningless
	TestCasesCreatedTargetGroupNames    map[string]bool //key: TargetGroupName; value: not in use, meaningless
	// TODO: instead of using one big list TestCasesCreatedK8sResource to track all created k8s resource,
	//  we should create different lists for different kind of k8s resource i.e., httproute have 1 list, service have another list etc.
	TestCasesCreatedK8sResource []client.Object
}

func NewFramework(ctx context.Context, log gwlog.Logger, testNamespace string) *Framework {

	addOptionalCRDs(testScheme)
	config.ConfigInit()
	controllerRuntimeConfig := controllerruntime.GetConfigOrDie()
	framework := &Framework{
		Client:                              lo.Must(client.New(controllerRuntimeConfig, client.Options{Scheme: testScheme})),
		LatticeClient:                       services.NewDefaultLattice(session.Must(session.NewSession()), config.Region), // region is currently hardcoded
		GrpcurlRunner:                       &v1.Pod{},
		ctx:                                 ctx,
		log:                                 log,
		k8sScheme:                           testScheme,
		namespace:                           testNamespace,
		controllerRuntimeConfig:             controllerRuntimeConfig,
		TestCasesCreatedServiceNetworkNames: make(map[string]bool),
		TestCasesCreatedServiceNames:        make(map[string]bool),
		TestCasesCreatedTargetGroupNames:    make(map[string]bool),
	}
	SetDefaultEventuallyTimeout(3 * time.Minute)
	SetDefaultEventuallyPollingInterval(10 * time.Second)
	return framework
}

func (env *Framework) ExpectToBeClean(ctx context.Context) {
	env.log.Info("Expecting the test environment to be clean")
	// Kubernetes API Objects
	parallel.ForEach(TestObjects, func(testObject TestObject, _ int) {
		defer GinkgoRecover()
		env.EventuallyExpectNoneFound(ctx, testObject.ListType)
	})

	retrievedServiceNetworkVpcAssociations, _ := env.LatticeClient.ListServiceNetworkVpcAssociationsAsList(ctx, &vpclattice.ListServiceNetworkVpcAssociationsInput{
		VpcIdentifier: aws.String(CurrentClusterVpcId),
	})
	env.log.Infof("Expect VPC used by current cluster has no ServiceNetworkVPCAssociation, if it does you should manually delete it")
	Expect(len(retrievedServiceNetworkVpcAssociations)).To(Equal(0))
	Eventually(func(g Gomega) {
		retrievedServiceNetworks, _ := env.LatticeClient.ListServiceNetworksAsList(ctx, &vpclattice.ListServiceNetworksInput{})
		for _, sn := range retrievedServiceNetworks {
			env.log.Infof("Found service network, checking if created by current EKS Cluster: %v", sn)
			g.Expect(*sn.Name).Should(Not(BeKeyOf(env.TestCasesCreatedServiceNetworkNames)))
			retrievedTags, err := env.LatticeClient.ListTagsForResourceWithContext(ctx, &vpclattice.ListTagsForResourceInput{
				ResourceArn: sn.Arn,
			})
			if err == nil { // for err != nil, it is possible that this service network own by other account, and it is shared to current account by RAM
				env.log.Infof("Found Tags for serviceNetwork %v tags: %v", *sn.Name, retrievedTags)

				value, ok := retrievedTags.Tags[lattice.K8SServiceNetworkOwnedByVPC]
				if ok {
					g.Expect(*value).To(Not(Equal(CurrentClusterVpcId)))
				}
			}
		}

		retrievedServices, _ := env.LatticeClient.ListServicesAsList(ctx, &vpclattice.ListServicesInput{})
		for _, service := range retrievedServices {
			env.log.Infof("Found service, checking if created by current EKS Cluster: %v", service)
			g.Expect(*service.Name).Should(Not(BeKeyOf(env.TestCasesCreatedServiceNames)))
			retrievedTags, err := env.LatticeClient.ListTagsForResourceWithContext(ctx, &vpclattice.ListTagsForResourceInput{
				ResourceArn: service.Arn,
			})
			if err == nil { // for err != nil, it is possible that this service own by other account, and it is shared to current account by RAM
				env.log.Infof("Found Tags for service %v tags: %v", *service.Name, retrievedTags)
				value, ok := retrievedTags.Tags[lattice.K8SServiceOwnedByVPC]
				if ok {
					g.Expect(*value).To(Not(Equal(CurrentClusterVpcId)))
				}
			}
		}

		retrievedTargetGroups, _ := env.LatticeClient.ListTargetGroupsAsList(ctx, &vpclattice.ListTargetGroupsInput{})
		for _, tg := range retrievedTargetGroups {
			env.log.Infof("Found TargetGroup: %s, checking if created by current EKS Cluster", *tg.Id)
			if tg.VpcIdentifier != nil && CurrentClusterVpcId != *tg.VpcIdentifier {
				env.log.Infof("Target group VPC Id: %s, does not match current EKS Cluster VPC Id: %s", *tg.VpcIdentifier, CurrentClusterVpcId)
				//This tg is not created by current EKS Cluster, skip it
				continue
			}
			retrievedTags, err := env.LatticeClient.ListTagsForResourceWithContext(ctx, &vpclattice.ListTagsForResourceInput{
				ResourceArn: tg.Arn,
			})
			if err == nil {
				env.log.Infof("Found Tags for tg %v tags: %v", *tg.Name, retrievedTags)
				tagValue, ok := retrievedTags.Tags[lattice.K8SParentRefTypeKey]
				if ok && *tagValue == lattice.K8SServiceExportType {
					env.log.Infof("TargetGroup: %s was created by k8s controller, by a ServiceExport", *tg.Id)
					//This tg is created by k8s controller, by a ServiceExport,
					//ServiceExport still have a known targetGroup leaking issue,
					//so we temporarily skip to verify whether ServiceExport created TargetGroup is deleted or not
					continue
				}
				g.Expect(env.TestCasesCreatedServiceNames).To(Not(ContainElements(BeKeyOf(*tg.Name))))
			}
		}
	}).Should(Succeed())
}

// if we still want this method, we should tag the things we create with the test suite
func (env *Framework) CleanTestEnvironment(ctx context.Context) {
	defer GinkgoRecover()
	env.log.Info("Cleaning the test environment")
	// Kubernetes API Objects
	namespaces := &v1.NamespaceList{}
	Expect(env.List(ctx, namespaces)).WithOffset(1).To(Succeed())
	// TODO: instead of using one big list TestCasesCreatedK8sResource to track all created k8s resource,
	//  we should create different lists for different kind of k8s resource i.e., httproute have 1 list, service have another list etc.
	for _, object := range env.TestCasesCreatedK8sResource {
		env.log.Infof("Deleting k8s resource %s %s/%s", reflect.TypeOf(object), object.GetNamespace(), object.GetName())
		env.Delete(ctx, object)
		//Ignore resource-not-found error here, as the test case logic itself could already clear the resources
	}

	//Theoretically, Deleting all k8s resource by above `env.Delete(ctx, object)`, will make controller delete all related VPC Lattice resource,
	//but the controller is still developing in the progress and may leaking some VPCLattice resource, need to invoke vpcLattice api to double confirm and delete leaking resource.
	env.DeleteAllFrameworkTracedServiceNetworks(ctx)
	env.DeleteAllFrameworkTracedVpcLatticeServices(ctx)
	env.DeleteAllFrameworkTracedTargetGroups(ctx)
	env.EventuallyExpectNotFound(ctx, env.TestCasesCreatedK8sResource...)
	env.TestCasesCreatedK8sResource = nil

}

func (env *Framework) ExpectCreated(ctx context.Context, objects ...client.Object) {
	for _, object := range objects {
		env.log.Infof("Creating %s %s/%s", reflect.TypeOf(object), object.GetNamespace(), object.GetName())
		Expect(env.Create(ctx, object)).WithOffset(1).To(Succeed())
	}
}

func (env *Framework) ExpectUpdated(ctx context.Context, objects ...client.Object) {
	for _, object := range objects {
		env.log.Infof("Updating %s %s/%s", reflect.TypeOf(object), object.GetNamespace(), object.GetName())
		Expect(env.Update(ctx, object)).WithOffset(1).To(Succeed())
	}
}

func (env *Framework) ExpectDeletedThenNotFound(ctx context.Context, objects ...client.Object) {
	env.ExpectDeleted(ctx, objects...)
	env.EventuallyExpectNotFound(ctx, objects...)
}

func (env *Framework) ExpectDeleted(ctx context.Context, objects ...client.Object) {
	for _, object := range objects {
		env.log.Infof("Deleting %s %s/%s", reflect.TypeOf(object), object.GetNamespace(), object.GetName())
		err := env.Delete(ctx, object)
		if err != nil {
			// not found is probably OK - means it was deleted elsewhere
			if !errors.IsNotFound(err) {
				Expect(err).ToNot(HaveOccurred())
			}
		}
	}
}

func (env *Framework) ExpectDeleteAllToSucceed(ctx context.Context, object client.Object, namespace string) {
	Expect(env.DeleteAllOf(ctx, object, client.InNamespace(namespace), client.HasLabels([]string{DiscoveryLabel}))).WithOffset(1).To(Succeed())
}

func (env *Framework) EventuallyExpectNotFound(ctx context.Context, objects ...client.Object) {
	Eventually(func(g Gomega) {
		for _, object := range objects {
			if object != nil {
				env.log.Infof("Checking whether %s %s/%s is not found", reflect.TypeOf(object), object.GetNamespace(), object.GetName())
				g.Expect(errors.IsNotFound(env.Get(ctx, client.ObjectKeyFromObject(object), object))).To(BeTrue())
			}
		}
		// Wait for 7 minutes at maximum just in case the k8sService deletion triggered targets draining time
		// and httproute deletion need to wait for that targets draining time finish then it can return
	}).WithTimeout(7 * time.Minute).WithOffset(1).Should(Succeed())
}

func (env *Framework) EventuallyExpectNoneFound(ctx context.Context, objectList client.ObjectList) {
	Eventually(func(g Gomega) {
		g.Expect(env.List(ctx, objectList, client.HasLabels([]string{DiscoveryLabel}))).To(Succeed())
		g.Expect(meta.ExtractList(objectList)).To(HaveLen(0), "Expected to not find any %q with label %q", reflect.TypeOf(objectList), DiscoveryLabel)
	}).WithOffset(1).Should(Succeed())
}

func (env *Framework) GetServiceNetwork(ctx context.Context, gateway *gateway_api_v1beta1.Gateway) *vpclattice.ServiceNetworkSummary {
	var found *vpclattice.ServiceNetworkSummary
	Eventually(func(g Gomega) {
		listServiceNetworksOutput, err := env.LatticeClient.ListServiceNetworksWithContext(ctx, &vpclattice.ListServiceNetworksInput{})
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(listServiceNetworksOutput.Items).ToNot(BeEmpty())
		for _, serviceNetwork := range listServiceNetworksOutput.Items {
			if lo.FromPtr(serviceNetwork.Name) == gateway.Name {
				found = serviceNetwork
			}
		}
		g.Expect(found).ToNot(BeNil())
	}).WithOffset(1).Should(Succeed())
	return found
}

func (env *Framework) GetVpcLatticeService(ctx context.Context, route core.Route) *vpclattice.ServiceSummary {
	var found *vpclattice.ServiceSummary
	serviceName := latticestore.LatticeServiceName(route.Name(), route.Namespace())
	Eventually(func(g Gomega) {
		listServicesOutput, err := env.LatticeClient.ListServicesWithContext(ctx, &vpclattice.ListServicesInput{})
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(listServicesOutput.Items).ToNot(BeEmpty())
		for _, service := range listServicesOutput.Items {
			if *service.Name == serviceName {
				found = service
			}
		}
		g.Expect(found).ToNot(BeNil())
		g.Expect(found.Status).To(Equal(lo.ToPtr(vpclattice.ServiceStatusActive)))
		g.Expect(found.DnsEntry).To(ContainSubstring(serviceName))
	}).WithOffset(1).Should(Succeed())

	return found
}

func (env *Framework) GetTargetGroup(ctx context.Context, service *v1.Service) *vpclattice.TargetGroupSummary {
	return env.GetTargetGroupWithProtocol(ctx, service, "http", "http1")
}

func (env *Framework) GetTargetGroupWithProtocol(ctx context.Context, service *v1.Service, protocol, protocolVersion string) *vpclattice.TargetGroupSummary {
	latticeTGName := fmt.Sprintf("%s-%s-%s",
		latticestore.TargetGroupName(service.Name, service.Namespace), protocol, protocolVersion)
	var found *vpclattice.TargetGroupSummary
	Eventually(func(g Gomega) {
		targetGroups, err := env.LatticeClient.ListTargetGroupsAsList(ctx, &vpclattice.ListTargetGroupsInput{})
		g.Expect(err).To(BeNil())
		for _, targetGroup := range targetGroups {
			if lo.FromPtr(targetGroup.Name) == latticeTGName {
				found = targetGroup
				break
			}
		}
		g.Expect(found).ToNot(BeNil())
		g.Expect(found.Status).To(Equal(lo.ToPtr(vpclattice.TargetGroupStatusActive)))
	}).WithOffset(1).Should(Succeed())
	return found
}

// TODO: Create a new function that only verifying deployment len(podList.Items)==*deployment.Spec.Replicas, and don't do lattice.ListTargets() api call
func (env *Framework) GetTargets(ctx context.Context, targetGroup *vpclattice.TargetGroupSummary, deployment *appsv1.Deployment) []*vpclattice.TargetSummary {
	var found []*vpclattice.TargetSummary
	Eventually(func(g Gomega) {
		podIps, retrievedTargets := GetTargets(targetGroup, deployment, env, ctx)

		targetIps := lo.Filter(retrievedTargets, func(target *vpclattice.TargetSummary, _ int) bool {
			return lo.Contains(podIps, *target.Id) &&
				(*target.Status == vpclattice.TargetStatusInitial ||
					*target.Status == vpclattice.TargetStatusHealthy)
		})

		g.Expect(retrievedTargets).Should(HaveLen(len(targetIps)))
		found = retrievedTargets
	}).WithPolling(time.Minute).WithTimeout(7 * time.Minute).Should(Succeed())
	return found
}

func (env *Framework) GetAllTargets(ctx context.Context, targetGroup *vpclattice.TargetGroupSummary, deployment *appsv1.Deployment) ([]string, []*vpclattice.TargetSummary) {
	return GetTargets(targetGroup, deployment, env, ctx)
}

func GetTargets(targetGroup *vpclattice.TargetGroupSummary, deployment *appsv1.Deployment, env *Framework, ctx context.Context) ([]string, []*vpclattice.TargetSummary) {
	env.log.Infoln("Trying to retrieve registered targets for targetGroup", targetGroup.Name)
	env.log.Infoln("deployment.Spec.Selector.MatchLabels:", deployment.Spec.Selector.MatchLabels)
	podList := &v1.PodList{}
	expectedMatchingLabels := make(map[string]string, len(deployment.Spec.Selector.MatchLabels))
	for k, v := range deployment.Spec.Selector.MatchLabels {
		expectedMatchingLabels[k] = v
	}
	expectedMatchingLabels[DiscoveryLabel] = "true"
	env.log.Infoln("Expected matching labels:", expectedMatchingLabels)
	Expect(env.List(ctx, podList, client.MatchingLabels(expectedMatchingLabels))).To(Succeed())
	Expect(podList.Items).To(HaveLen(int(*deployment.Spec.Replicas)))
	retrievedTargets, err := env.LatticeClient.ListTargetsAsList(ctx, &vpclattice.ListTargetsInput{TargetGroupIdentifier: targetGroup.Id})
	Expect(err).To(BeNil())

	podIps := utils.SliceMap(podList.Items, func(pod v1.Pod) string { return pod.Status.PodIP })

	return podIps, retrievedTargets
}

func (env *Framework) VerifyTargetGroupNotFound(tg *vpclattice.TargetGroupSummary) {
	Eventually(func(g Gomega) {
		retrievedTargetGroup, err := env.LatticeClient.GetTargetGroup(&vpclattice.GetTargetGroupInput{
			TargetGroupIdentifier: tg.Id,
		})
		g.Expect(retrievedTargetGroup.Id).To(BeNil())
		g.Expect(err).To(Not(BeNil()))
		if err != nil {
			if aerr, ok := err.(awserr.Error); ok {
				g.Expect(aerr.Code()).To(Equal(vpclattice.ErrCodeResourceNotFoundException))
			}
		}
	}).Should(Succeed())
}

func (env *Framework) IsVpcAssociatedWithServiceNetwork(ctx context.Context, vpcId string, serviceNetwork *vpclattice.ServiceNetworkSummary) (bool, error) {
	env.log.Infof("IsVpcAssociatedWithServiceNetwork vpcId:%v serviceNetwork: %v \n", vpcId, serviceNetwork)
	vpcAssociations, err := env.LatticeClient.ListServiceNetworkVpcAssociationsAsList(ctx, &vpclattice.ListServiceNetworkVpcAssociationsInput{
		ServiceNetworkIdentifier: serviceNetwork.Id,
		VpcIdentifier:            &vpcId,
	})
	if err != nil {
		return false, err
	}
	if len(vpcAssociations) != 1 {
		return false, fmt.Errorf("Expect to have one VpcServiceNetworkAssociation len(vpcAssociations): %d", len(vpcAssociations))
	}
	association := vpcAssociations[0]
	if *association.Status != vpclattice.ServiceNetworkVpcAssociationStatusActive {
		return false, fmt.Errorf("Current cluster should have one Active status association *association.Status: %s, err: %w", *association.Status, err)
	}
	return true, nil
}

func (env *Framework) AreAllLatticeTargetsHealthy(ctx context.Context, tg *vpclattice.TargetGroupSummary) (bool, error) {
	env.log.Infof("Checking whether AreAllLatticeTargetsHealthy for targetGroup: %v", tg)
	targets, err := env.LatticeClient.ListTargetsAsList(ctx, &vpclattice.ListTargetsInput{TargetGroupIdentifier: tg.Id})
	if err != nil {
		return false, err
	}
	for _, target := range targets {
		env.log.Infof("Checking target: %v", target)
		if *target.Status != vpclattice.TargetStatusHealthy {
			return false, nil
		}
	}
	return true, nil
}

func (env *Framework) DeleteAllFrameworkTracedServiceNetworks(ctx aws.Context) {
	env.log.Infof("DeleteAllFrameworkTracedServiceNetworks %v", env.TestCasesCreatedServiceNetworkNames)
	sns, err := env.LatticeClient.ListServiceNetworksAsList(ctx, &vpclattice.ListServiceNetworksInput{})
	Expect(err).ToNot(HaveOccurred())
	filteredSns := lo.Filter(sns, func(sn *vpclattice.ServiceNetworkSummary, _ int) bool {
		_, ok := env.TestCasesCreatedServiceNames[*sn.Name]
		return ok
	})
	snIds := lo.Map(filteredSns, func(svc *vpclattice.ServiceNetworkSummary, _ int) *string {
		return svc.Id
	})
	var serviceNetworkIdsWithRemainingAssociations []*string
	for _, snId := range snIds {
		_, err := env.LatticeClient.DeleteServiceNetworkWithContext(ctx, &vpclattice.DeleteServiceNetworkInput{
			ServiceNetworkIdentifier: snId,
		})
		if err != nil {
			if aerr, ok := err.(awserr.Error); ok {
				switch aerr.Code() {
				case vpclattice.ErrCodeResourceNotFoundException:
					continue
				case vpclattice.ErrCodeConflictException:
					serviceNetworkIdsWithRemainingAssociations = append(serviceNetworkIdsWithRemainingAssociations, snId)
				}
			}
		}
	}

	var allServiceNetworkVpcAssociationIdsToBeDeleted []*string
	for _, snIdWithRemainingAssociations := range serviceNetworkIdsWithRemainingAssociations {
		associations, err := env.LatticeClient.ListServiceNetworkVpcAssociationsAsList(ctx, &vpclattice.ListServiceNetworkVpcAssociationsInput{
			ServiceNetworkIdentifier: snIdWithRemainingAssociations,
		})
		Expect(err).ToNot(HaveOccurred())

		snvaIds := lo.Map(associations, func(association *vpclattice.ServiceNetworkVpcAssociationSummary, _ int) *string {
			return association.Id
		})
		allServiceNetworkVpcAssociationIdsToBeDeleted = append(allServiceNetworkVpcAssociationIdsToBeDeleted, snvaIds...)
	}

	for _, snvaId := range allServiceNetworkVpcAssociationIdsToBeDeleted {
		_, err := env.LatticeClient.DeleteServiceNetworkVpcAssociationWithContext(ctx, &vpclattice.DeleteServiceNetworkVpcAssociationInput{
			ServiceNetworkVpcAssociationIdentifier: snvaId,
		})
		Expect(err).ToNot(HaveOccurred())
	}

	var allServiceNetworkServiceAssociationIdsToBeDeleted []*string

	for _, snIdWithRemainingAssociations := range serviceNetworkIdsWithRemainingAssociations {
		associations, err := env.LatticeClient.ListServiceNetworkServiceAssociationsAsList(ctx, &vpclattice.ListServiceNetworkServiceAssociationsInput{
			ServiceNetworkIdentifier: snIdWithRemainingAssociations,
		})
		Expect(err).ToNot(HaveOccurred())

		snsaIds := lo.Map(associations, func(association *vpclattice.ServiceNetworkServiceAssociationSummary, _ int) *string {
			return association.Id
		})
		allServiceNetworkServiceAssociationIdsToBeDeleted = append(allServiceNetworkServiceAssociationIdsToBeDeleted, snsaIds...)
	}

	for _, snsaId := range allServiceNetworkServiceAssociationIdsToBeDeleted {
		_, err := env.LatticeClient.DeleteServiceNetworkServiceAssociationWithContext(ctx, &vpclattice.DeleteServiceNetworkServiceAssociationInput{
			ServiceNetworkServiceAssociationIdentifier: snsaId,
		})
		Expect(err).ToNot(HaveOccurred())
	}

	Eventually(func(g Gomega) {
		for _, snvaId := range allServiceNetworkVpcAssociationIdsToBeDeleted {
			_, err := env.LatticeClient.GetServiceNetworkVpcAssociationWithContext(ctx, &vpclattice.GetServiceNetworkVpcAssociationInput{
				ServiceNetworkVpcAssociationIdentifier: snvaId,
			})
			if err != nil {
				g.Expect(err.(awserr.Error).Code()).To(Equal(vpclattice.ErrCodeResourceNotFoundException))
			}
		}
		for _, snsaId := range allServiceNetworkServiceAssociationIdsToBeDeleted {
			_, err := env.LatticeClient.GetServiceNetworkServiceAssociationWithContext(ctx, &vpclattice.GetServiceNetworkServiceAssociationInput{
				ServiceNetworkServiceAssociationIdentifier: snsaId,
			})
			if err != nil {
				g.Expect(err.(awserr.Error).Code()).To(Equal(vpclattice.ErrCodeResourceNotFoundException))
			}
		}
	}).Should(Succeed())

	for _, snId := range serviceNetworkIdsWithRemainingAssociations {
		env.LatticeClient.DeleteServiceNetworkWithContext(ctx, &vpclattice.DeleteServiceNetworkInput{
			ServiceNetworkIdentifier: snId,
		})
	}

	env.TestCasesCreatedServiceNetworkNames = make(map[string]bool)
}

// In the VPC Lattice backend code, delete VPC Lattice services will also make all its listeners and rules to be deleted asynchronously
func (env *Framework) DeleteAllFrameworkTracedVpcLatticeServices(ctx aws.Context) {
	env.log.Infoln("DeleteAllFrameworkTracedVpcLatticeServices", env.TestCasesCreatedServiceNames)
	services, err := env.LatticeClient.ListServicesAsList(ctx, &vpclattice.ListServicesInput{})
	Expect(err).ToNot(HaveOccurred())
	filteredServices := lo.Filter(services, func(service *vpclattice.ServiceSummary, _ int) bool {
		_, ok := env.TestCasesCreatedServiceNames[*service.Name]
		return ok
	})
	serviceIds := lo.Map(filteredServices, func(svc *vpclattice.ServiceSummary, _ int) *string {
		return svc.Id
	})
	var serviceWithRemainingAssociations []*string
	for _, serviceId := range serviceIds {
		_, err := env.LatticeClient.DeleteServiceWithContext(ctx, &vpclattice.DeleteServiceInput{
			ServiceIdentifier: serviceId,
		})
		if err != nil {
			if aerr, ok := err.(awserr.Error); ok {
				switch aerr.Code() {
				case vpclattice.ErrCodeResourceNotFoundException:
					delete(env.TestCasesCreatedServiceNames, *serviceId)
					continue
				case vpclattice.ErrCodeConflictException:
					serviceWithRemainingAssociations = append(serviceWithRemainingAssociations, serviceId)
				}
			}

		}
	}
	var allServiceNetworkServiceAssociationIdsToBeDeleted []*string

	for _, serviceIdWithRemainingAssociations := range serviceWithRemainingAssociations {

		associations, err := env.LatticeClient.ListServiceNetworkServiceAssociationsAsList(ctx, &vpclattice.ListServiceNetworkServiceAssociationsInput{
			ServiceIdentifier: serviceIdWithRemainingAssociations,
		})
		Expect(err).ToNot(HaveOccurred())

		snsaIds := lo.Map(associations, func(association *vpclattice.ServiceNetworkServiceAssociationSummary, _ int) *string {
			return association.Id
		})
		allServiceNetworkServiceAssociationIdsToBeDeleted = append(allServiceNetworkServiceAssociationIdsToBeDeleted, snsaIds...)
	}

	for _, snsaId := range allServiceNetworkServiceAssociationIdsToBeDeleted {
		_, err := env.LatticeClient.DeleteServiceNetworkServiceAssociationWithContext(ctx, &vpclattice.DeleteServiceNetworkServiceAssociationInput{
			ServiceNetworkServiceAssociationIdentifier: snsaId,
		})
		if err != nil {
			Expect(err.(awserr.Error).Code()).To(Equal(vpclattice.ErrCodeResourceNotFoundException))
		}
	}

	Eventually(func(g Gomega) {
		for _, snsaId := range allServiceNetworkServiceAssociationIdsToBeDeleted {
			_, err := env.LatticeClient.GetServiceNetworkServiceAssociationWithContext(ctx, &vpclattice.GetServiceNetworkServiceAssociationInput{
				ServiceNetworkServiceAssociationIdentifier: snsaId,
			})
			if err != nil {
				g.Expect(err.(awserr.Error).Code()).To(Equal(vpclattice.ErrCodeResourceNotFoundException))
			}
		}
	}).Should(Succeed())

	for _, serviceId := range serviceWithRemainingAssociations {
		env.LatticeClient.DeleteServiceWithContext(ctx, &vpclattice.DeleteServiceInput{
			ServiceIdentifier: serviceId,
		})
	}
	env.TestCasesCreatedServiceNames = make(map[string]bool)
}

func (env *Framework) DeleteAllFrameworkTracedTargetGroups(ctx aws.Context) {
	targetGroups, err := env.LatticeClient.ListTargetGroupsAsList(ctx, &vpclattice.ListTargetGroupsInput{})
	Expect(err).ToNot(HaveOccurred())
	filteredTgs := lo.Filter(targetGroups, func(targetGroup *vpclattice.TargetGroupSummary, _ int) bool {
		for key, _ := range env.TestCasesCreatedTargetGroupNames {
			if strings.HasPrefix(*targetGroup.Name, key) {
				return true
			}
		}
		return false
	})
	tgIds := lo.Map(filteredTgs, func(targetGroup *vpclattice.TargetGroupSummary, _ int) *string {
		return targetGroup.Id
	})

	env.log.Infoln("Number of traced target groups to delete is:", len(tgIds))

	var tgsToDeregister []string
	for _, tgId := range tgIds {
		env.log.Infoln("Attempting to delete target group: ", tgId)

		_, err := env.LatticeClient.DeleteTargetGroup(&vpclattice.DeleteTargetGroupInput{
			TargetGroupIdentifier: tgId,
		})
		if err != nil {
			tgsToDeregister = append(tgsToDeregister, *tgId)
		} else {
			env.log.Infoln("Deleted target group: ", tgId)
		}
	}

	// next try to deregister targets
	var tgsToDelete []string
	for _, tgId := range tgsToDeregister {
		targetSummaries, err := env.LatticeClient.ListTargetsAsList(ctx, &vpclattice.ListTargetsInput{
			TargetGroupIdentifier: &tgId,
		})

		if err.(awserr.Error).Code() == vpclattice.ErrCodeResourceNotFoundException {
			env.log.Infoln("Target group already deleted: ", tgId)
			continue
		}

		Expect(err).ToNot(HaveOccurred())
		tgsToDelete = append(tgsToDelete, tgId)
		if len(targetSummaries) > 0 {
			var targets []*vpclattice.Target = lo.Map(targetSummaries, func(targetSummary *vpclattice.TargetSummary, _ int) *vpclattice.Target {
				return &vpclattice.Target{
					Id:   targetSummary.Id,
					Port: targetSummary.Port,
				}
			})
			env.LatticeClient.DeregisterTargetsWithContext(ctx, &vpclattice.DeregisterTargetsInput{
				TargetGroupIdentifier: &tgId,
				Targets:               targets,
			})
		}
	}

	if len(tgsToDelete) > 0 {
		env.log.Infoln("Need to wait for draining targets to be deregistered", tgsToDelete)
		// After initiating the DeregisterTargets call, the Targets will be in `draining` status for the next 5 minutes,
		// And VPC lattice backend will run a background job to completely delete the targets within 6 minutes at maximum in total.
		Eventually(func(g Gomega) {
			env.log.Infoln("Trying to clear Target group", tgsToDelete, "need to wait for draining targets to be deregistered")

			for _, tgId := range tgsToDelete {
				_, err := env.LatticeClient.DeleteTargetGroupWithContext(ctx, &vpclattice.DeleteTargetGroupInput{
					TargetGroupIdentifier: &tgId,
				})
				if err != nil {
					g.Expect(err.(awserr.Error).Code()).To(Equal(vpclattice.ErrCodeResourceNotFoundException))
				}
			}
		}).WithPolling(15 * time.Second).WithTimeout(7 * time.Minute).Should(Succeed())

	}
	env.TestCasesCreatedServiceNames = make(map[string]bool)
}

func (env *Framework) GetLatticeServiceHttpsListenerNonDefaultRules(ctx context.Context, vpcLatticeService *vpclattice.ServiceSummary) ([]*vpclattice.GetRuleOutput, error) {

	listListenerResp, err := env.LatticeClient.ListListenersWithContext(ctx, &vpclattice.ListListenersInput{
		ServiceIdentifier: vpcLatticeService.Id,
	})
	if err != nil {
		return nil, err
	}

	httpsListenerId := ""
	for _, item := range listListenerResp.Items {
		if strings.Contains(*item.Name, "https") {
			httpsListenerId = *item.Id
			break
		}
	}
	if httpsListenerId == "" {
		return nil, fmt.Errorf("expect having 1 https listener for lattice service %s, but got 0", *vpcLatticeService.Id)
	}
	listRulesResp, err := env.LatticeClient.ListRulesWithContext(ctx, &vpclattice.ListRulesInput{
		ListenerIdentifier: &httpsListenerId,
		ServiceIdentifier:  vpcLatticeService.Id,
	})
	if err != nil {
		return nil, err
	}
	nonDefaultRules := utils.SliceFilter(listRulesResp.Items, func(rule *vpclattice.RuleSummary) bool {
		return rule.IsDefault != nil && *rule.IsDefault == false
	})

	nonDefaultRuleIds := utils.SliceMap(nonDefaultRules, func(rule *vpclattice.RuleSummary) *string {
		return rule.Id
	})

	var retrievedRules []*vpclattice.GetRuleOutput
	for _, ruleId := range nonDefaultRuleIds {
		rule, err := env.LatticeClient.GetRuleWithContext(ctx, &vpclattice.GetRuleInput{
			ServiceIdentifier:  vpcLatticeService.Id,
			ListenerIdentifier: &httpsListenerId,
			RuleIdentifier:     ruleId,
		})
		if err != nil {
			return nil, err
		}
		retrievedRules = append(retrievedRules, rule)
	}
	return retrievedRules, nil
}

func (env *Framework) GetVpcLatticeServiceDns(httpRouteName string, httpRouteNamespace string) string {
	env.log.Infoln("GetVpcLatticeServiceDns: ", httpRouteName, httpRouteNamespace)
	httproute := gateway_api_v1beta1.HTTPRoute{}
	env.Get(env.ctx, types.NamespacedName{Name: httpRouteName, Namespace: httpRouteNamespace}, &httproute)
	vpcLatticeServiceDns := httproute.Annotations[controllers.LatticeAssignedDomainName]
	return vpcLatticeServiceDns
}

type RunGrpcurlCmdOptions struct {
	GrpcServerHostName  string
	GrpcServerPort      string
	Service             string
	Method              string
	Headers             [][2]string // a slice of string tuple
	ReqParamsJsonString string
	UseTLS              bool
}

// https://github.com/fullstorydev/grpcurl
// https://gallery.ecr.aws/a0j4q9e4/grpcurl-runner
func (env *Framework) RunGrpcurlCmd(opts RunGrpcurlCmdOptions) (string, string, error) {
	env.log.Infoln("RunGrpcurlCmd")
	Expect(env.GrpcurlRunner).To(Not(BeNil()))

	tlsOption := ""
	if !opts.UseTLS {
		tlsOption = "-plaintext"
	}

	headers := ""
	for _, tuple := range opts.Headers {
		headers += fmt.Sprintf("-H '%s: %s' ", tuple[0], tuple[1])
	}

	reqParams := ""
	if opts.ReqParamsJsonString != "" {
		reqParams = fmt.Sprintf("-d '%s'", opts.ReqParamsJsonString)
	}

	cmd := fmt.Sprintf("/grpcurl "+
		"-proto /protos/addsvc.proto "+
		"-proto /protos/grpcbin.proto "+
		"-proto /protos/helloworld.proto "+
		"%s %s %s %s:%s %s/%s",
		tlsOption,
		headers,
		reqParams,
		opts.GrpcServerHostName,
		opts.GrpcServerPort,
		opts.Service,
		opts.Method)

	stdoutStr, stderrStr, err := env.PodExec(env.GrpcurlRunner.Namespace, env.GrpcurlRunner.Name, cmd, false)
	return stdoutStr, stderrStr, err

}

func (env *Framework) SleepForRouteDeletion() {
	time.Sleep(30 * time.Second)
}
