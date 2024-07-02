/*
Copyright 2017 The Kubernetes Authors.

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

package registry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	log "github.com/sirupsen/logrus"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
	"sigs.k8s.io/external-dns/provider"

	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	CRDRegistryOwnerLabel      string = "externaldns.k8s.io/owner"
	CRDRegistryRecordNameLabel string = "externaldns.k8s.io/record-name"
	CRDRegistryRecordTypeLabel string = "externaldns.k8s.io/record-type"
	CRDRegistryIdentifierLabel string = "externaldns.k8s.io/identifier"
)

type CRDConfig struct {
	KubeConfig   string
	APIServerURL string
	APIVersion   string
	Kind         string
}

type CRDClient struct {
	scheme   *runtime.Scheme
	codec    runtime.ParameterCodec
	resource *metav1.APIResource
	rest.Interface
}

// CRDRegistry implements registry interface with ownership implemented via associated custom resource records (DSNEntry)
type CRDRegistry struct {
	client    *CRDClient
	namespace string
	provider  provider.Provider
	ownerID   string // refers to the owner id of the current instance

	// cache the records in memory and update on an interval instead.
	recordsCache            []*endpoint.Endpoint
	recordsCacheRefreshTime time.Time
	cacheInterval           time.Duration
}

// NewCRDClientForAPIVersionKind return rest client for the given apiVersion and kind of the CRD
func NewCRDClientForAPIVersionKind(client kubernetes.Interface, kubeConfig, apiServerURL, apiVersion, kind string) (*CRDClient, error) {
	if kubeConfig == "" {
		if _, err := os.Stat(clientcmd.RecommendedHomeFile); err == nil {
			kubeConfig = clientcmd.RecommendedHomeFile
		}
	}

	config, err := clientcmd.BuildConfigFromFlags(apiServerURL, kubeConfig)
	if err != nil {
		return nil, err
	}

	groupVersion, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return nil, err
	}
	apiResourceList, err := client.Discovery().ServerResourcesForGroupVersion(groupVersion.String())
	if err != nil {
		return nil, fmt.Errorf("error listing resources in GroupVersion %q: %w", groupVersion.String(), err)
	}

	var crdAPIResource *metav1.APIResource
	for _, apiResource := range apiResourceList.APIResources {
		if apiResource.Kind == kind {
			crdAPIResource = &apiResource
			break
		}
	}
	if crdAPIResource == nil {
		return nil, fmt.Errorf("unable to find Resource Kind %q in GroupVersion %q", kind, apiVersion)
	}

	scheme := runtime.NewScheme()
	scheme.AddKnownTypes(groupVersion,
		&DNSEntry{},
		&DNSEntryList{},
	)
	metav1.AddToGroupVersion(scheme, groupVersion)

	config.ContentConfig.GroupVersion = &groupVersion
	config.APIPath = "/apis"
	config.NegotiatedSerializer = serializer.WithoutConversionCodecFactory{CodecFactory: serializer.NewCodecFactory(scheme)}

	crdClient, err := rest.UnversionedRESTClientFor(config)
	if err != nil {
		return nil, err
	}

	return &CRDClient{scheme: scheme, resource: crdAPIResource, codec: runtime.NewParameterCodec(scheme), Interface: crdClient}, nil
}

// NewCRDRegistry returns new CRDRegistry object
func NewCRDRegistry(provider provider.Provider, crdClient *CRDClient, ownerID string, cacheInterval time.Duration, namespace string) (*CRDRegistry, error) {
	if ownerID == "" {
		return nil, errors.New("owner id cannot be empty")
	}

	return &CRDRegistry{
		client:        crdClient,
		namespace:     namespace,
		provider:      provider,
		ownerID:       ownerID,
		cacheInterval: cacheInterval,
	}, nil
}

func (im *CRDRegistry) GetDomainFilter() endpoint.DomainFilter {
	return im.provider.GetDomainFilter()
}

func (im *CRDRegistry) OwnerID() string {
	return im.ownerID
}

// Records returns the current records from the registry
func (im *CRDRegistry) Records(ctx context.Context) ([]*endpoint.Endpoint, error) {
	// If we have the zones cached AND we have refreshed the cache since the
	// last given interval, then just use the cached results.
	if im.recordsCache != nil && time.Since(im.recordsCacheRefreshTime) < im.cacheInterval {
		log.Debug("Using cached records.")
		return im.recordsCache, nil
	}

	records, err := im.provider.Records(ctx)
	if err != nil {
		return nil, err
	}

	endpoints := []*endpoint.Endpoint{}

	for _, record := range records {
		// AWS Alias records have "new" format encoded as type "cname"
		if isAlias, found := record.GetProviderSpecificProperty("alias"); found && isAlias == "true" && record.RecordType == endpoint.RecordTypeA {
			record.RecordType = endpoint.RecordTypeCNAME
		}

		endpoints = append(endpoints, record)
	}

	// Update the cache.
	if im.cacheInterval > 0 {
		im.recordsCache = endpoints
		im.recordsCacheRefreshTime = time.Now()
	}

	return endpoints, nil
}

// ApplyChanges updates dns provider with the changes and creates/updates/delete a DNSEntry
// custom resource as the registry entry.
func (im *CRDRegistry) ApplyChanges(ctx context.Context, changes *plan.Changes) error {
	filteredChanges := &plan.Changes{
		Create:    changes.Create,
		UpdateNew: endpoint.FilterEndpointsByOwnerID(im.ownerID, changes.UpdateNew),
		UpdateOld: endpoint.FilterEndpointsByOwnerID(im.ownerID, changes.UpdateOld),
		Delete:    endpoint.FilterEndpointsByOwnerID(im.ownerID, changes.Delete),
	}

	for _, r := range filteredChanges.Create {
		entry := &DNSEntry{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-%s", r.DNSName, im.OwnerID()),
				Namespace: im.namespace,
				Labels: map[string]string{
					CRDRegistryOwnerLabel:      im.OwnerID(),
					CRDRegistryRecordNameLabel: r.DNSName,
					CRDRegistryRecordTypeLabel: r.RecordType,
					CRDRegistryIdentifierLabel: r.SetIdentifier,
				},
			},
			Spec: DNSEntrySpec{
				Endpoints: []*endpoint.Endpoint{r},
			},
		}
		result := im.client.Post().Namespace(im.namespace).Body(&entry).Do(ctx)
		if err := result.Error(); err != nil {
			// It could be possible that a record already exists if a previous apply change happened
			// and there was an error while creating those records through the provider. For that reason,
			// this error is ignored, all others will be surfaced back to the user
			if !k8sErrors.IsAlreadyExists(err) {
				return err
			}
		}

		if im.cacheInterval > 0 {
			im.addToCache(r)
		}
	}

	for _, r := range filteredChanges.Delete {
		var entries DNSEntryList
		opts := metav1.ListOptions{
			LabelSelector: fmt.Sprintf("%s=%s,%s=%s", CRDRegistryIdentifierLabel, r.SetIdentifier, CRDRegistryOwnerLabel, im.ownerID),
		}

		err := im.client.Get().Namespace(im.namespace).Resource(im.client.resource.Name).VersionedParams(&opts, im.client.codec).Do(ctx).Into(&entries)
		if err != nil {
			return err
		}

		// While this is a list, it is expected that this call will return 0 or 1 entries.
		for _, e := range entries.Items {
			result := im.client.Delete().Namespace(im.namespace).Resource(im.client.resource.Name).Name(e.Name).Do(ctx)
			if err := result.Error(); err != nil {
				// Ignore not found as it's a benign error, the entry record isn't present and it's the end goal here, to remove
				// all entries. All other errors should surface back to the user.
				if !k8sErrors.IsNotFound(err) {
					return err
				}
			}
		}

		// Update existing DNS entries to reflect the newest change.
		for i, e := range filteredChanges.UpdateNew {
			old := filteredChanges.UpdateOld[i]

			var entries DNSEntryList
			opts := metav1.ListOptions{
				LabelSelector: fmt.Sprintf("%s=%s,%s=%s", CRDRegistryIdentifierLabel, old.SetIdentifier, CRDRegistryOwnerLabel, im.ownerID),
			}

			err := im.client.Get().Namespace(im.namespace).Resource(im.client.resource.Name).VersionedParams(&opts, im.client.codec).Do(ctx).Into(&entries)
			if err != nil {
				return err
			}

			for _, entry := range entries.Items {
				entry.Spec.Endpoints = []*endpoint.Endpoint{e}
				result := im.client.Put().Namespace(im.namespace).Resource(im.client.resource.Name).Name(entry.Name).Body(&entry).Do(ctx)
				if err := result.Error(); err != nil {
					return err
				}
			}
		}

		if im.cacheInterval > 0 {
			im.removeFromCache(r)
		}
	}

	return im.provider.ApplyChanges(ctx, filteredChanges)
}

// AdjustEndpoints modifies the endpoints as needed by the specific provider
func (im *CRDRegistry) AdjustEndpoints(endpoints []*endpoint.Endpoint) ([]*endpoint.Endpoint, error) {
	return im.provider.AdjustEndpoints(endpoints)
}

func (im *CRDRegistry) addToCache(ep *endpoint.Endpoint) {
	if im.recordsCache != nil {
		im.recordsCache = append(im.recordsCache, ep)
	}
}

func (im *CRDRegistry) removeFromCache(ep *endpoint.Endpoint) {
	if im.recordsCache == nil || ep == nil {
		return
	}

	for i, e := range im.recordsCache {
		if e.DNSName == ep.DNSName && e.RecordType == ep.RecordType && e.SetIdentifier == ep.SetIdentifier && e.Targets.Same(ep.Targets) {
			// We found a match delete the endpoint from the cache.
			im.recordsCache = append(im.recordsCache[:i], im.recordsCache[i+1:]...)
			return
		}
	}
}

// DNSEntrySpec defines the desired state of DNSEndpoint
// +kubebuilder:object:generate=true
type DNSEntrySpec struct {
	Endpoints []*endpoint.Endpoint `json:"endpoints,omitempty"`
}

// DNSEntryStatus defines the observed state of DNSENtry
// +kubebuilder:object:generate=true
type DNSEntryStatus struct {
	// The generation observed by the external-dns controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// DNSEntry is a contract that a user-specified CRD must implement to be used as a source for external-dns.
// The user-specified CRD should also have the status sub-resource.
// +k8s:openapi-gen=true
// +groupName=externaldns.k8s.io
// +kubebuilder:resource:path=dnsentries
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +versionName=v1alpha1

type DNSEntry struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DNSEntrySpec   `json:"spec,omitempty"`
	Status DNSEntryStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// DNSEndpointList is a list of DNSEndpoint objects
type DNSEntryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DNSEntry `json:"items"`
}
