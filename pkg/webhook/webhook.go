// Copyright (c) 2018 Intel Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/golang/glog"
	cniv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/pkg/errors"
	multus "gopkg.in/k8snetworkplumbingwg/multus-cni.v4/pkg/types"

	admissionv1 "k8s.io/api/admission/v1"
	v1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/k8snetworkplumbingwg/network-resources-injector/pkg/controlswitches"
	netcache "github.com/k8snetworkplumbingwg/network-resources-injector/pkg/tools"
	"github.com/k8snetworkplumbingwg/network-resources-injector/pkg/types"
	"github.com/k8snetworkplumbingwg/network-resources-injector/pkg/userdefinedinjections"
)

type hugepageResourceData struct {
	ResourceName  string
	ContainerName string
	Path          string
}

const (
	networksAnnotationKey       = "k8s.v1.cni.cncf.io/networks"
	nodeSelectorKey             = "k8s.v1.cni.cncf.io/nodeSelector"
	defaultNetworkAnnotationKey = "v1.multus-cni.io/default-network"
)

var (
	HugepageRegex         = regexp.MustCompile(`^hugepages-(.+)$`)
	clientset             kubernetes.Interface
	nadCache              netcache.NetAttachDefCacheService
	userDefinedInjections *userdefinedinjections.UserDefinedInjections
	controlSwitches       *controlswitches.ControlSwitches
)

func SetControlSwitches(activeConfiguration *controlswitches.ControlSwitches) {
	controlSwitches = activeConfiguration
}

func SetUserInjectionStructure(injections *userdefinedinjections.UserDefinedInjections) {
	userDefinedInjections = injections
}

func prepareAdmissionReviewResponse(allowed bool, message string, ar *admissionv1.AdmissionReview) error {
	if ar.Request != nil {
		ar.Response = &admissionv1.AdmissionResponse{
			UID:     ar.Request.UID,
			Allowed: allowed,
		}
		if message != "" {
			ar.Response.Result = &metav1.Status{
				Message: message,
			}
		}
		return nil
	}
	return errors.New("received empty AdmissionReview request")
}

func readAdmissionReview(req *http.Request, w http.ResponseWriter) (*admissionv1.AdmissionReview, int, error) {
	var body []byte

	if req.Body != nil {
		req.Body = http.MaxBytesReader(w, req.Body, 1<<20)
		if data, err := io.ReadAll(req.Body); err == nil {
			body = data
		}
	}

	if len(body) == 0 {
		err := errors.New("Error reading HTTP request: empty body")
		glog.Errorf("%s", err)
		return nil, http.StatusBadRequest, err
	}

	/* validate HTTP request headers */
	contentType := req.Header.Get("Content-Type")
	if contentType != "application/json" {
		err := errors.Errorf("Invalid Content-Type='%s', expected 'application/json'", contentType)
		glog.Errorf("%v", err)
		return nil, http.StatusUnsupportedMediaType, err
	}

	/* read AdmissionReview from the request body */
	ar, err := deserializeAdmissionReview(body)
	if err != nil {
		err := errors.Wrap(err, "error deserializing AdmissionReview")
		glog.Errorf("%v", err)
		return nil, http.StatusBadRequest, err
	}

	return ar, http.StatusOK, nil
}

func deserializeAdmissionReview(body []byte) (*admissionv1.AdmissionReview, error) {
	ar := &admissionv1.AdmissionReview{}
	runtimeScheme := runtime.NewScheme()
	codecs := serializer.NewCodecFactory(runtimeScheme)
	deserializer := codecs.UniversalDeserializer()
	_, _, err := deserializer.Decode(body, nil, ar)

	/* Decode() won't return an error if the data wasn't actual AdmissionReview */
	if err == nil && ar.TypeMeta.Kind != "AdmissionReview" {
		err = errors.New("received object is not an AdmissionReview")
	}

	return ar, err
}

func deserializePod(ar *admissionv1.AdmissionReview) (corev1.Pod, error) {
	/* unmarshal Pod from AdmissionReview request */
	pod := corev1.Pod{}
	err := json.Unmarshal(ar.Request.Object.Raw, &pod)
	if pod.ObjectMeta.Namespace != "" {
		return pod, err
	}

	// AdmissionRequest contains an optional Namespace field
	if ar.Request.Namespace != "" {
		pod.ObjectMeta.Namespace = ar.Request.Namespace
		return pod, nil
	}

	ownerRef := pod.ObjectMeta.OwnerReferences
	if ownerRef != nil && len(ownerRef) > 0 {
		namespace, err := getNamespaceFromOwnerReference(pod.ObjectMeta.OwnerReferences[0])
		if err != nil {
			return pod, err
		}
		pod.ObjectMeta.Namespace = namespace
	}

	// pod.ObjectMeta.Namespace may still be empty at this point,
	// but there is a chance that net-attach-def annotation contains
	// a valid namespace
	return pod, err
}

func getNamespaceFromOwnerReference(ownerRef metav1.OwnerReference) (namespace string, err error) {
	namespace = ""
	switch ownerRef.Kind {
	case "ReplicaSet":
		var replicaSets *v1.ReplicaSetList
		replicaSets, err = clientset.AppsV1().ReplicaSets("").List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return
		}
		for _, replicaSet := range replicaSets.Items {
			if replicaSet.ObjectMeta.Name == ownerRef.Name && replicaSet.ObjectMeta.UID == ownerRef.UID {
				namespace = replicaSet.ObjectMeta.Namespace
				err = nil
				break
			}
		}
	case "DaemonSet":
		var daemonSets *v1.DaemonSetList
		daemonSets, err = clientset.AppsV1().DaemonSets("").List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return
		}
		for _, daemonSet := range daemonSets.Items {
			if daemonSet.ObjectMeta.Name == ownerRef.Name && daemonSet.ObjectMeta.UID == ownerRef.UID {
				namespace = daemonSet.ObjectMeta.Namespace
				err = nil
				break
			}
		}
	case "StatefulSet":
		var statefulSets *v1.StatefulSetList
		statefulSets, err = clientset.AppsV1().StatefulSets("").List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return
		}
		for _, statefulSet := range statefulSets.Items {
			if statefulSet.ObjectMeta.Name == ownerRef.Name && statefulSet.ObjectMeta.UID == ownerRef.UID {
				namespace = statefulSet.ObjectMeta.Namespace
				err = nil
				break
			}
		}
	case "ReplicationController":
		var replicationControllers *corev1.ReplicationControllerList
		replicationControllers, err = clientset.CoreV1().ReplicationControllers("").List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return
		}
		for _, replicationController := range replicationControllers.Items {
			if replicationController.ObjectMeta.Name == ownerRef.Name && replicationController.ObjectMeta.UID == ownerRef.UID {
				namespace = replicationController.ObjectMeta.Namespace
				err = nil
				break
			}
		}
	default:
		glog.Infof("owner reference kind is not supported: %v, using default namespace", ownerRef.Kind)
		namespace = "default"
		return
	}

	if namespace == "" {
		err = errors.New("pod namespace is not found")
	}

	return

}

func toSafeJsonPatchKey(in string) string {
	out := strings.Replace(in, "~", "~0", -1)
	out = strings.Replace(out, "/", "~1", -1)
	return out
}

func parsePodNetworkSelections(podNetworks, defaultNamespace string) ([]*multus.NetworkSelectionElement, error) {
	var networkSelections []*multus.NetworkSelectionElement

	if len(podNetworks) == 0 {
		err := errors.New("empty string passed as network selection elements list")
		glog.Error(err)
		return nil, err
	}

	/* try to parse as JSON array */
	err := json.Unmarshal([]byte(podNetworks), &networkSelections)

	/* if failed, try to parse as comma separated */
	if err != nil {
		glog.Infof("'%s' is not in JSON format: %s... trying to parse as comma separated network selections list", podNetworks, err)
		for _, networkSelection := range strings.Split(podNetworks, ",") {
			networkSelection = strings.TrimSpace(networkSelection)
			networkSelectionElement, err := parsePodNetworkSelectionElement(networkSelection, defaultNamespace)
			if err != nil {
				err := errors.Wrap(err, "error parsing network selection element")
				glog.Error(err)
				return nil, err
			}
			networkSelections = append(networkSelections, networkSelectionElement)
		}
	}

	/* fill missing namespaces with default value */
	for _, networkSelection := range networkSelections {
		if networkSelection.Namespace == "" {
			if defaultNamespace == "" {
				// Ignore the AdmissionReview request when the following conditions are met:
				// 1) net-attach-def annotation doesn't contain a valid namespace
				// 2) defaultNamespace retrieved from admission request is empty
				// Pod admission would fail in subsquent call "getNetworkAttachmentDefinition"
				// if no namespace is specified. We don't want to fail the pod creation
				// in such case since it is possible that pod is not a SR-IOV pod
				glog.Warningf("The admission request doesn't contain a valid namespace, ignoring...")
				return nil, nil
			} else {
				networkSelection.Namespace = defaultNamespace
			}
		}
	}

	return networkSelections, nil
}

func parsePodNetworkSelectionElement(selection, defaultNamespace string) (*multus.NetworkSelectionElement, error) {
	var namespace, name, netInterface string
	var networkSelectionElement *multus.NetworkSelectionElement

	units := strings.Split(selection, "/")
	switch len(units) {
	case 1:
		namespace = defaultNamespace
		name = units[0]
	case 2:
		namespace = units[0]
		name = units[1]
	default:
		err := errors.Errorf("invalid network selection element - more than one '/' rune in: '%s'", selection)
		glog.Info(err)
		return networkSelectionElement, err
	}

	units = strings.Split(name, "@")
	switch len(units) {
	case 1:
		name = units[0]
		netInterface = ""
	case 2:
		name = units[0]
		netInterface = units[1]
	default:
		err := errors.Errorf("invalid network selection element - more than one '@' rune in: '%s'", selection)
		glog.Info(err)
		return networkSelectionElement, err
	}

	validNameRegex, _ := regexp.Compile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	for _, unit := range []string{namespace, name, netInterface} {
		ok := validNameRegex.MatchString(unit)
		if !ok && len(unit) > 0 {
			err := errors.Errorf("at least one of the network selection units is invalid: error found at '%s'", unit)
			glog.Info(err)
			return networkSelectionElement, err
		}
	}

	networkSelectionElement = &multus.NetworkSelectionElement{
		Namespace:        namespace,
		Name:             name,
		InterfaceRequest: netInterface,
	}

	return networkSelectionElement, nil
}

func getNetworkAttachmentDefinition(namespace, name string) (*cniv1.NetworkAttachmentDefinition, error) {
	path := fmt.Sprintf("/apis/k8s.cni.cncf.io/v1/namespaces/%s/network-attachment-definitions/%s", namespace, name)
	rawNetworkAttachmentDefinition, err := clientset.ExtensionsV1beta1().RESTClient().Get().AbsPath(path).DoRaw(context.TODO())
	if err != nil {
		err := errors.Wrapf(err, "could not get Network Attachment Definition %s/%s", namespace, name)
		glog.Error(err)
		return nil, err
	}

	networkAttachmentDefinition := cniv1.NetworkAttachmentDefinition{}
	json.Unmarshal(rawNetworkAttachmentDefinition, &networkAttachmentDefinition)

	return &networkAttachmentDefinition, nil
}

func parseNetworkAttachDefinition(net *multus.NetworkSelectionElement, reqs map[string]int64, nsMap map[string]string) (map[string]int64, map[string]string, error) {
	/* for each network in annotation ask API server for network-attachment-definition */
	annotationsMap := nadCache.Get(net.Namespace, net.Name)
	if annotationsMap == nil {
		glog.Infof("cache entry not found, retrieving network attachment definition '%s/%s' from api server", net.Namespace, net.Name)
		networkAttachmentDefinition, err := getNetworkAttachmentDefinition(net.Namespace, net.Name)
		if err != nil {
			/* if doesn't exist: deny pod */
			reason := errors.Wrapf(err, "could not find network attachment definition '%s/%s'", net.Namespace, net.Name)
			glog.Error(reason)
			return reqs, nsMap, reason
		}
		annotationsMap = networkAttachmentDefinition.GetAnnotations()
	}
	glog.Infof("network attachment definition '%s/%s' found", net.Namespace, net.Name)

	/* network object exists, so check if it contains resourceName annotation */
	for _, networkResourceNameKey := range controlSwitches.GetResourceNameKeys() {
		if resourceName, exists := annotationsMap[networkResourceNameKey]; exists {
			/* add resource to map/increment if it was already there */
			reqs[resourceName]++
			glog.Infof("resource '%s' needs to be requested for network '%s/%s'", resourceName, net.Namespace, net.Name)
		} else {
			glog.Infof("network '%s/%s' doesn't use custom resources, skipping...", net.Namespace, net.Name)
		}
	}

	/* parse the net-attach-def annotations for node selector label and add it to the desiredNsMap */
	if ns, exists := annotationsMap[nodeSelectorKey]; exists {
		nsNameValue := strings.Split(ns, "=")
		nsNameValueLen := len(nsNameValue)
		if nsNameValueLen > 2 {
			reason := fmt.Errorf("node selector in net-attach-def %s has more than one label", net.Name)
			glog.Error(reason)
			return reqs, nsMap, reason
		} else if nsNameValueLen == 2 {
			nsMap[strings.TrimSpace(nsNameValue[0])] = strings.TrimSpace(nsNameValue[1])
		} else {
			nsMap[strings.TrimSpace(ns)] = ""
		}
	}

	return reqs, nsMap, nil
}

func handleValidationError(w http.ResponseWriter, ar *admissionv1.AdmissionReview, orgErr error) {
	err := prepareAdmissionReviewResponse(false, orgErr.Error(), ar)
	if err != nil {
		err := errors.Wrap(err, "error preparing AdmissionResponse")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeResponse(w, ar)
}

func writeResponse(w http.ResponseWriter, ar *admissionv1.AdmissionReview) {
	glog.Infof("sending response to the Kubernetes API server")
	resp, _ := json.Marshal(ar)
	w.Write(resp)
}

func patchEmptyResources(patch []types.JsonPatchOperation, containerIndex uint, key string) []types.JsonPatchOperation {
	patch = append(patch, types.JsonPatchOperation{
		Operation: "add",
		Path:      "/spec/containers/" + fmt.Sprintf("%d", containerIndex) + "/resources/" + toSafeJsonPatchKey(key),
		Value:     corev1.ResourceList{},
	})
	return patch
}

func addVolDownwardAPI(patch []types.JsonPatchOperation, hugepageResourceList []hugepageResourceData, pod *corev1.Pod) []types.JsonPatchOperation {
	if len(pod.Spec.Volumes) == 0 {
		patch = append(patch, types.JsonPatchOperation{
			Operation: "add",
			Path:      "/spec/volumes",
			Value:     []corev1.Volume{},
		})
	}

	dAPIItems := []corev1.DownwardAPIVolumeFile{}

	if pod.Labels != nil && len(pod.Labels) > 0 {
		labels := corev1.ObjectFieldSelector{
			FieldPath: "metadata.labels",
		}
		dAPILabels := corev1.DownwardAPIVolumeFile{
			Path:     types.LabelsPath,
			FieldRef: &labels,
		}
		dAPIItems = append(dAPIItems, dAPILabels)
	}

	if pod.Annotations != nil && len(pod.Annotations) > 0 {
		annotations := corev1.ObjectFieldSelector{
			FieldPath: "metadata.annotations",
		}
		dAPIAnnotations := corev1.DownwardAPIVolumeFile{
			Path:     types.AnnotationsPath,
			FieldRef: &annotations,
		}
		dAPIItems = append(dAPIItems, dAPIAnnotations)
	}

	for _, hugepageResource := range hugepageResourceList {
		hugepageSelector := corev1.ResourceFieldSelector{
			Resource:      hugepageResource.ResourceName,
			ContainerName: hugepageResource.ContainerName,
			Divisor:       *resource.NewQuantity(1*1024*1024, resource.BinarySI),
		}
		dAPIHugepage := corev1.DownwardAPIVolumeFile{
			Path:             hugepageResource.Path,
			ResourceFieldRef: &hugepageSelector,
		}
		dAPIItems = append(dAPIItems, dAPIHugepage)
	}

	dAPIVolSource := corev1.DownwardAPIVolumeSource{
		Items: dAPIItems,
	}
	volSource := corev1.VolumeSource{
		DownwardAPI: &dAPIVolSource,
	}
	vol := corev1.Volume{
		Name:         "podnetinfo",
		VolumeSource: volSource,
	}

	patch = append(patch, types.JsonPatchOperation{
		Operation: "add",
		Path:      "/spec/volumes/-",
		Value:     vol,
	})

	return patch
}

func addVolumeMount(patch []types.JsonPatchOperation, containers []corev1.Container) []types.JsonPatchOperation {
	vm := corev1.VolumeMount{
		Name:      "podnetinfo",
		ReadOnly:  true,
		MountPath: types.DownwardAPIMountPath,
	}
	for containerIndex, container := range containers {
		if len(container.VolumeMounts) == 0 {
			patch = append(patch, types.JsonPatchOperation{
				Operation: "add",
				Path:      "/spec/containers/" + strconv.Itoa(containerIndex) + "/volumeMounts",
				Value:     []corev1.VolumeMount{},
			})
		}
		patch = append(patch, types.JsonPatchOperation{
			Operation: "add",
			Path:      "/spec/containers/" + strconv.Itoa(containerIndex) + "/volumeMounts/-",
			Value:     vm,
		})
	}

	return patch
}

func createVolPatch(patch []types.JsonPatchOperation, hugepageResourceList []hugepageResourceData, pod *corev1.Pod) []types.JsonPatchOperation {
	patch = addVolumeMount(patch, pod.Spec.Containers)
	patch = addVolDownwardAPI(patch, hugepageResourceList, pod)
	return patch
}

func addEnvVar(patch []types.JsonPatchOperation, containerIndex int, firstElement bool,
	envName string, envVal string) []types.JsonPatchOperation {

	env := corev1.EnvVar{
		Name:  envName,
		Value: envVal,
	}

	if firstElement {
		patch = append(patch, types.JsonPatchOperation{
			Operation: "add",
			Path:      "/spec/containers/" + fmt.Sprintf("%d", containerIndex) + "/env",
			Value:     []corev1.EnvVar{env},
		})
	} else {
		patch = append(patch, types.JsonPatchOperation{
			Operation: "add",
			Path:      "/spec/containers/" + fmt.Sprintf("%d", containerIndex) + "/env/-",
			Value:     env,
		})
	}

	return patch
}

func createEnvPatch(patch []types.JsonPatchOperation, container *corev1.Container,
	containerIndex int, envName string, envVal string) []types.JsonPatchOperation {

	// Determine if requested ENV already exists
	found := false
	firstElement := false
	if len(container.Env) != 0 {
		for _, env := range container.Env {
			if env.Name == envName {
				found = true
				if env.Value != envVal {
					glog.Warningf("Error, adding env '%s', name existed but value different: '%s' != '%s'",
						envName, env.Value, envVal)
				}
				break
			}
		}
	} else {
		firstElement = true
	}

	if !found {
		patch = addEnvVar(patch, containerIndex, firstElement, envName, envVal)
	}
	return patch
}

func createNodeSelectorPatch(patch []types.JsonPatchOperation, existing map[string]string, desired map[string]string) []types.JsonPatchOperation {
	targetMap := make(map[string]string)
	if existing != nil {
		for k, v := range existing {
			targetMap[k] = v
		}
	}
	if desired != nil {
		for k, v := range desired {
			targetMap[k] = v
		}
	}
	if len(targetMap) == 0 {
		return patch
	}
	patch = append(patch, types.JsonPatchOperation{
		Operation: "add",
		Path:      "/spec/nodeSelector",
		Value:     targetMap,
	})
	return patch
}

func createResourcePatch(patch []types.JsonPatchOperation, Containers []corev1.Container, resourceRequests map[string]int64) []types.JsonPatchOperation {
	/* check whether resources paths exists in the first container and add as the first patches if missing */
	if len(Containers[0].Resources.Requests) == 0 {
		patch = patchEmptyResources(patch, 0, "requests")
	}
	if len(Containers[0].Resources.Limits) == 0 {
		patch = patchEmptyResources(patch, 0, "limits")
	}

	for resourceName := range resourceRequests {
		for _, container := range Containers {
			if _, exists := container.Resources.Limits[corev1.ResourceName(resourceName)]; exists {
				delete(resourceRequests, resourceName)
			}
			if _, exists := container.Resources.Requests[corev1.ResourceName(resourceName)]; exists {
				delete(resourceRequests, resourceName)
			}
		}
	}

	resourceList := *getResourceList(resourceRequests)

	for resource, quantity := range resourceList {
		patch = appendResource(patch, resource.String(), quantity, quantity)
	}

	return patch
}

func updateResourcePatch(patch []types.JsonPatchOperation, Containers []corev1.Container, resourceRequests map[string]int64) []types.JsonPatchOperation {
	var existingrequestsMap map[corev1.ResourceName]resource.Quantity
	var existingLimitsMap map[corev1.ResourceName]resource.Quantity

	if len(Containers[0].Resources.Requests) == 0 {
		patch = patchEmptyResources(patch, 0, "requests")
	} else {
		existingrequestsMap = Containers[0].Resources.Requests
	}
	if len(Containers[0].Resources.Limits) == 0 {
		patch = patchEmptyResources(patch, 0, "limits")
	} else {
		existingLimitsMap = Containers[0].Resources.Limits
	}

	resourceList := *getResourceList(resourceRequests)

	for resourceName, quantity := range resourceList {
		reqQuantity := quantity
		limitQuantity := quantity
		if value, ok := existingrequestsMap[resourceName]; ok {
			reqQuantity.Add(value)
		}
		if value, ok := existingLimitsMap[resourceName]; ok {
			limitQuantity.Add(value)
		}
		patch = appendResource(patch, resourceName.String(), reqQuantity, limitQuantity)
	}

	return patch
}

func appendResource(patch []types.JsonPatchOperation, resourceName string, reqQuantity, limitQuantity resource.Quantity) []types.JsonPatchOperation {
	patch = append(patch, types.JsonPatchOperation{
		Operation: "add",
		Path:      "/spec/containers/0/resources/requests/" + toSafeJsonPatchKey(resourceName),
		Value:     reqQuantity,
	})
	patch = append(patch, types.JsonPatchOperation{
		Operation: "add",
		Path:      "/spec/containers/0/resources/limits/" + toSafeJsonPatchKey(resourceName),
		Value:     limitQuantity,
	})

	return patch
}

func getResourceList(resourceRequests map[string]int64) *corev1.ResourceList {
	resourceList := corev1.ResourceList{}
	for name, number := range resourceRequests {
		resourceList[corev1.ResourceName(name)] = *resource.NewQuantity(number, resource.DecimalSI)
	}

	return &resourceList
}

func appendAddAnnotPatch(patch []types.JsonPatchOperation, pod corev1.Pod, userDefinedPatch []types.JsonPatchOperation) []types.JsonPatchOperation {
	annotations := make(map[string]string)
	patchOp := types.JsonPatchOperation{
		Operation: "add",
		Path:      "/metadata/annotations",
		Value:     annotations,
	}

	for _, p := range userDefinedPatch {
		if p.Path == "/metadata/annotations" && p.Operation == "add" {
			//loop over user defined injected annotations key-value pairs
			for k, v := range p.Value.(map[string]interface{}) {
				if _, exists := annotations[k]; exists {
					glog.Warningf("ignoring duplicate user defined injected annotation: %s: %s", k, v.(string))
				} else {
					annotations[k] = v.(string)
				}
			}
		}
	}

	if len(annotations) > 0 {
		// attempt to add existing pod annotation but do not override
		for k, v := range pod.ObjectMeta.Annotations {
			if _, exists := annotations[k]; !exists {
				annotations[k] = v
			}
		}
		patch = append(patch, patchOp)
	}

	return patch
}

func appendUserDefinedPatch(patch []types.JsonPatchOperation, pod corev1.Pod, userDefinedPatch []types.JsonPatchOperation) []types.JsonPatchOperation {
	//Add operation for annotations is currently only supported
	return appendAddAnnotPatch(patch, pod, userDefinedPatch)
}

func getNetworkSelections(annotationKey string, pod corev1.Pod, userDefinedPatch []types.JsonPatchOperation) (string, bool) {
	// User defined annotateKey takes precedence than userDefined injections
	glog.Infof("search %s in original pod annotations", annotationKey)
	nets, exists := pod.ObjectMeta.Annotations[annotationKey]
	if exists {
		glog.Infof("%s is defined in original pod annotations", annotationKey)
		return nets, exists
	}

	glog.Infof("search %s in user-defined injections", annotationKey)
	// userDefinedPatch may contain user defined net-attach-defs
	if len(userDefinedPatch) > 0 {
		for _, p := range userDefinedPatch {
			if p.Operation == "add" && p.Path == "/metadata/annotations" {
				for k, v := range p.Value.(map[string]interface{}) {
					if k == annotationKey {
						glog.Infof("%s is found in user-defined annotations", annotationKey)
						return v.(string), true
					}
				}
			}
		}
	}
	glog.Infof("%s is not found in either pod annotations or user-defined injections", annotationKey)
	return "", false
}

func processHugepagesForDownwardAPI(patch []types.JsonPatchOperation, containers []corev1.Container) ([]types.JsonPatchOperation, []hugepageResourceData) {
	var hugepageResourceList []hugepageResourceData

	for containerIndex, container := range containers {
		found := false

		// Check requests
		if len(container.Resources.Requests) != 0 {
			for resourceName, quantity := range container.Resources.Requests {
				if matches := HugepageRegex.FindStringSubmatch(string(resourceName)); matches != nil && !quantity.IsZero() {
					hugepageSize := matches[1]
					hugepageResource := hugepageResourceData{
						ResourceName:  "requests." + string(resourceName),
						ContainerName: container.Name,
						Path:          fmt.Sprintf("hugepages_%s_request_%s", strings.ReplaceAll(hugepageSize, "i", ""), container.Name),
					}
					hugepageResourceList = append(hugepageResourceList, hugepageResource)
					found = true
				}
			}
		}

		// Check limits
		if len(container.Resources.Limits) != 0 {
			for resourceName, quantity := range container.Resources.Limits {
				if matches := HugepageRegex.FindStringSubmatch(string(resourceName)); matches != nil && !quantity.IsZero() {
					hugepageSize := matches[1]
					hugepageResource := hugepageResourceData{
						ResourceName:  "limits." + string(resourceName),
						ContainerName: container.Name,
						Path:          fmt.Sprintf("hugepages_%s_limit_%s", strings.ReplaceAll(hugepageSize, "i", ""), container.Name),
					}
					hugepageResourceList = append(hugepageResourceList, hugepageResource)
					found = true
				}
			}
		}

		// If Hugepages are being added to Downward API, add the
		// 'container.Name' as an environment variable to the container
		// so container knows its name and can process hugepages properly.
		if found {
			patch = createEnvPatch(patch, &container, containerIndex,
				types.EnvNameContainerName, container.Name)
		}
	}

	return patch, hugepageResourceList
}

// MutateHandler handles AdmissionReview requests and sends responses back to the K8s API server
func MutateHandler(w http.ResponseWriter, req *http.Request) {
	glog.Infof("Received mutation request. Features status: %s", controlSwitches.GetAllFeaturesState())
	var err error

	/* read AdmissionReview from the HTTP request */
	ar, httpStatus, err := readAdmissionReview(req, w)
	if err != nil {
		http.Error(w, err.Error(), httpStatus)
		return
	}

	/* read pod annotations */
	/* if networks missing skip everything */
	pod, err := deserializePod(ar)
	if err != nil {
		handleValidationError(w, ar, err)
		return
	}
	glog.Infof("AdmissionReview request received for pod %s/%s", pod.ObjectMeta.Namespace, pod.ObjectMeta.Name)

	userDefinedPatch, err := userDefinedInjections.CreateUserDefinedPatch(pod)
	if err != nil {
		glog.Warningf("failed to create user-defined injection patch for pod %s/%s, err: %v",
			pod.ObjectMeta.Namespace, pod.ObjectMeta.Name, err)
	}

	defaultNetSelection, defExist := getNetworkSelections(defaultNetworkAnnotationKey, pod, userDefinedPatch)
	additionalNetSelections, addExists := getNetworkSelections(networksAnnotationKey, pod, userDefinedPatch)

	if defExist || addExists {
		/* map of resources request needed by a pod and a number of them */
		resourceRequests := make(map[string]int64)

		/* map of node labels on which pod needs to be scheduled*/
		desiredNsMap := make(map[string]string)

		if defaultNetSelection != "" {
			defNetwork, err := parsePodNetworkSelections(defaultNetSelection, pod.ObjectMeta.Namespace)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if len(defNetwork) == 1 {
				resourceRequests, desiredNsMap, err = parseNetworkAttachDefinition(defNetwork[0], resourceRequests, desiredNsMap)
				if err != nil {
					err = prepareAdmissionReviewResponse(false, err.Error(), ar)
					if err != nil {
						glog.Errorf("error preparing AdmissionReview response for pod %s/%s, error: %v",
							pod.ObjectMeta.Namespace, pod.ObjectMeta.Name, err)
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					writeResponse(w, ar)
					return
				}
			}
		}
		if additionalNetSelections != "" {
			/* unmarshal list of network selection objects */
			networks, err := parsePodNetworkSelections(additionalNetSelections, pod.ObjectMeta.Namespace)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			for _, n := range networks {
				resourceRequests, desiredNsMap, err = parseNetworkAttachDefinition(n, resourceRequests, desiredNsMap)
				if err != nil {
					err = prepareAdmissionReviewResponse(false, err.Error(), ar)
					if err != nil {
						glog.Errorf("error preparing AdmissionReview response for pod %s/%s, error: %v",
							pod.ObjectMeta.Namespace, pod.ObjectMeta.Name, err)
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					writeResponse(w, ar)
					return
				}
			}
			glog.Infof("pod %s/%s has resource requests: %v and node selectors: %v", pod.ObjectMeta.Namespace,
				pod.ObjectMeta.Name, resourceRequests, desiredNsMap)
		}

		/* patch with custom resources requests and limits */
		err = prepareAdmissionReviewResponse(true, "allowed", ar)
		if err != nil {
			glog.Errorf("error preparing AdmissionReview response for pod %s/%s, error: %v",
				pod.ObjectMeta.Namespace, pod.ObjectMeta.Name, err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var patch []types.JsonPatchOperation
		if len(resourceRequests) == 0 {
			glog.Infof("pod %s/%s doesn't need any custom network resources", pod.ObjectMeta.Namespace, pod.ObjectMeta.Name)
		} else {
			if controlSwitches.IsHonorExistingResourcesEnabled() {
				patch = updateResourcePatch(patch, pod.Spec.Containers, resourceRequests)
			} else {
				patch = createResourcePatch(patch, pod.Spec.Containers, resourceRequests)
			}

			// Determine if hugepages are being requested for a given container,
			// and if so, expose the value to the container via Downward API.
			var hugepageResourceList []hugepageResourceData
			if controlSwitches.IsHugePagedownAPIEnabled() {
				patch, hugepageResourceList = processHugepagesForDownwardAPI(patch, pod.Spec.Containers)
			}
			patch = createVolPatch(patch, hugepageResourceList, &pod)
			patch = appendUserDefinedPatch(patch, pod, userDefinedPatch)
		}
		patch = createNodeSelectorPatch(patch, pod.Spec.NodeSelector, desiredNsMap)
		glog.Infof("patch after all mutations: %v for pod %s/%s", patch, pod.ObjectMeta.Namespace, pod.ObjectMeta.Name)

		patchBytes, _ := json.Marshal(patch)
		ar.Response.Patch = patchBytes
		ar.Response.PatchType = func() *admissionv1.PatchType {
			pt := admissionv1.PatchTypeJSONPatch
			return &pt
		}()
	} else {
		/* network annotation not provided or empty */
		glog.Infof("pod %s/%s spec doesn't have network annotations. Skipping...", pod.ObjectMeta.Namespace, pod.ObjectMeta.Name)
		err = prepareAdmissionReviewResponse(true, "Pod spec doesn't have network annotations. Skipping...", ar)
		if err != nil {
			glog.Errorf("error preparing AdmissionReview response for pod %s/%s, error: %v",
				pod.ObjectMeta.Namespace, pod.ObjectMeta.Name, err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	writeResponse(w, ar)
}

// SetNetAttachDefCache sets up the net attach def cache service
func SetNetAttachDefCache(cache netcache.NetAttachDefCacheService) {
	nadCache = cache
}

// SetupInClusterClient setups K8s client to communicate with the API server
func SetupInClusterClient() kubernetes.Interface {
	/* setup Kubernetes API client */
	config, err := rest.InClusterConfig()
	if err != nil {
		glog.Fatal(err)
	}
	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		glog.Fatal(err)
	}
	return clientset
}
