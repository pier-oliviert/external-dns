package registry

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	runtime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
	"sigs.k8s.io/external-dns/provider/inmemory"
)

type CRDSuite struct {
	suite.Suite
}

func (suite *CRDSuite) SetupTest() {
}

// The endpoints needs to be part of the zone otherwise it will be filtered out.
func inMemoryProviderWithEntries(t *testing.T, ctx context.Context, zone string, endpoints ...*endpoint.Endpoint) *inmemory.InMemoryProvider {
	p := inmemory.NewInMemoryProvider(inmemory.InMemoryInitZones([]string{zone}))

	err := p.ApplyChanges(ctx, &plan.Changes{
		Create: endpoints,
	})

	if err != nil {
		t.Fatal("Could not create an in memory provider", err)
	}

	return p
}

func TestCRDSource(t *testing.T) {
	suite.Run(t, new(CRDSuite))
	t.Run("Interface", testCRDSourceImplementsSource)
	t.Run("Constructor", testConstructor)
	t.Run("Records", testRecords)
	t.Run("ApplyChanges", testApplyChanges)
}

// testCRDSourceImplementsSource tests that crdSource is a valid Source.
func testCRDSourceImplementsSource(t *testing.T) {
	require.Implements(t, (*Registry)(nil), new(CRDRegistry))
}

func testConstructor(t *testing.T) {
	_, err := NewCRDRegistry(nil, nil, "", time.Second, "default")
	if err == nil {
		t.Error("Expected a new registry to return an error when no ownerID are specified")
	}

	_, err = NewCRDRegistry(nil, nil, "ownerID", time.Second, "")
	if err == nil {
		t.Error("Expected a new registry to return an error when no namespace are specified")
	}

	_, err = NewCRDRegistry(nil, nil, "ownerID", time.Second, "namespace")
	if err != nil {
		t.Error("Expected registry to be initialized without error when providing an owner id and a namespace", err)
	}
}

func testRecords(t *testing.T) {
	ctx := context.Background()
	t.Run("use the cache if within the time interval", func(t *testing.T) {
		registry := &CRDRegistry{
			recordsCacheRefreshTime: time.Now(),
			cacheInterval:           time.Hour,
			recordsCache: []*endpoint.Endpoint{{
				DNSName:    "cached.mytestdomain.io",
				RecordType: "A",
				Targets:    []string{"127.0.0.1"},
			}},
		}
		endpoints, err := registry.Records(ctx)
		if err != nil {
			t.Error(err)
		}

		if len(endpoints) != 1 {
			t.Error("expected only 1 record from the cache, got: ", len(endpoints))
		}

		if endpoints[0].DNSName != "cached.mytestdomain.io" {
			t.Error("expected DNS Name to be the cached value got: ", endpoints[0].DNSName)
		}
	})

	t.Run("ALIAS records are converted to CNAME", func(t *testing.T) {
		e := []*endpoint.Endpoint{
			{
				DNSName:    "foo.mytestdomain.io",
				RecordType: "A",
				Targets:    []string{"127.0.0.1"},
				ProviderSpecific: []endpoint.ProviderSpecificProperty{{
					Name:  "alias",
					Value: "true",
				}},
			},
		}
		provider := inMemoryProviderWithEntries(t, ctx, "mytestdomain.io", e...)

		registry := &CRDRegistry{
			provider:  provider,
			namespace: "default",
			client:    NewMockCRDClient(),
			ownerID:   "test",
		}

		endpoints, err := registry.Records(ctx)
		if err != nil {
			t.Error(err)
		}
		t.Logf("Endpoints: %#v", endpoints[0])

		if endpoints[0].RecordType != "CNAME" {
			t.Error("Expected record type to be changed from ALIAS to CNAME: ", endpoints[0].RecordType)
		}
	})

	t.Run("Add existing labels from registry to the record from the provider", func(t *testing.T) {
		provider := inMemoryProviderWithEntries(t, ctx, "mytestdomain.io")
		responses := map[mockRequest]mockResponse{}
		responses[mockRequest{
			method:    "GET",
			namespace: "default",
		}] = mockResponse{}
		client := NewMockCRDClient(responses)

		registry := &CRDRegistry{
			provider:  provider,
			namespace: "default",
			client:    client,
			ownerID:   "test",
		}

		endpoints, err := registry.Records(ctx)
		if err != nil {
			t.Error(err)
		}
		t.Logf("Endpoints: %#v", endpoints[0])

		if endpoints[0].RecordType != "CNAME" {
			t.Error("Expected record type to be changed from ALIAS to CNAME: ", endpoints[0].RecordType)
		}

	})
}

func testApplyChanges(t *testing.T) {

}

// Mocks
type mockClient struct {
	mockResponses map[mockRequest]mockResponse
}

func NewMockCRDClient(responses map[mockRequest]mockResponse) CRDClient {
	if responses == nil {
		responses = map[mockRequest]mockResponse{}
	}

	return &mockClient{
		mockResponses: responses,
	}
}

func (m *mockClient) MockResponses() {
}

func (m *mockClient) Get() CRDRequest {
	return &mockRequest{}
}

func (m *mockClient) List() CRDRequest {
	return &mockRequest{}
}

func (m *mockClient) Put() CRDRequest {
	return &mockRequest{}
}

func (m *mockClient) Post() CRDRequest {
	return &mockRequest{}
}

func (m *mockClient) Delete() CRDRequest {
	return &mockRequest{}
}

type mockRequest struct {
	method    string
	namespace string
	name      string
}

func (mr *mockRequest) Name(string) CRDRequest {
	return mr
}
func (mr *mockRequest) Namespace(string) CRDRequest {
	return mr
}

func (mr *mockRequest) Body(interface{}) CRDRequest {
	return mr
}

func (mr *mockRequest) Params(runtime.Object) CRDRequest {
	return mr
}

func (mr *mockRequest) Do(ctx context.Context) CRDResult {
	return &mockResponse{}
}

type mockResponse struct {
	content interface{}
}

func (mr *mockResponse) Error() error {
	return nil
}

func (mr *mockResponse) Into(runtime.Object) error {
	return nil
}
