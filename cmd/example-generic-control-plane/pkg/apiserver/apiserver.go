// Package apiserver provides the generic control plane API server.
package apiserver

import (
	"context"
	"fmt"
	"net"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/endpoints/openapi"
	"k8s.io/apiserver/pkg/registry/generic"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"
	genericapiserver "k8s.io/apiserver/pkg/server"
	serveroptions "k8s.io/apiserver/pkg/server/options"
	"k8s.io/apiserver/pkg/storage/storagebackend"
	"sigs.k8s.io/controller-runtime/pkg/client"

	examplev1alpha1 "github.com/kausality-io/kausality/cmd/example-generic-control-plane/pkg/apis/example/v1alpha1"
	kausalityAdmission "github.com/kausality-io/kausality/cmd/example-generic-control-plane/pkg/admission"
	"github.com/kausality-io/kausality/cmd/example-generic-control-plane/pkg/registry/example/widget"
	"github.com/kausality-io/kausality/cmd/example-generic-control-plane/pkg/registry/example/widgetset"
	"github.com/kausality-io/kausality/pkg/policy"
)

var (
	// Scheme is the runtime scheme for the API server.
	Scheme = runtime.NewScheme()
	// Codecs provides encoder/decoder for the scheme.
	Codecs = serializer.NewCodecFactory(Scheme)
)

func init() {
	// Register example types
	examplev1alpha1.AddToScheme(Scheme)
}

// Config holds the configuration for the API server.
type Config struct {
	// EtcdServers is the list of etcd server addresses.
	EtcdServers []string
	// BindAddress is the address to bind to.
	BindAddress string
	// BindPort is the port to bind to.
	BindPort int
	// Log is the logger.
	Log logr.Logger
	// PolicyResolver is the kausality policy resolver.
	PolicyResolver policy.Resolver
	// Client is the controller-runtime client for kausality (can be nil for simple use).
	Client client.Client
}

// Server is the API server.
type Server struct {
	GenericAPIServer *genericapiserver.GenericAPIServer
	log              logr.Logger
	listener         net.Listener
}

// New creates a new API server.
func New(cfg Config) (*Server, error) {
	// Create generic server config
	genericConfig := genericapiserver.NewRecommendedConfig(Codecs)

	// Configure serving
	if cfg.BindAddress == "" {
		cfg.BindAddress = "127.0.0.1"
	}
	if cfg.BindPort == 0 {
		cfg.BindPort = 8443
	}

	// Create listener
	listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", cfg.BindAddress, cfg.BindPort))
	if err != nil {
		return nil, fmt.Errorf("failed to create listener: %w", err)
	}

	// Configure secure serving with self-signed cert
	secureServing := serveroptions.NewSecureServingOptions()
	secureServing.Listener = listener
	secureServing.ServerCert.GeneratedCert = nil // Will generate self-signed
	if err := secureServing.MaybeDefaultWithSelfSignedCerts("localhost", nil, []net.IP{net.ParseIP("127.0.0.1")}); err != nil {
		listener.Close()
		return nil, fmt.Errorf("failed to configure self-signed certs: %w", err)
	}
	if err := secureServing.ApplyTo(&genericConfig.Config.SecureServing); err != nil {
		listener.Close()
		return nil, fmt.Errorf("failed to apply secure serving: %w", err)
	}

	// Disable authentication - allow all
	genericConfig.Authentication.Authenticator = nil

	// Disable authorization - allow all
	genericConfig.Authorization.Authorizer = allowAllAuthorizer{}

	// Configure etcd storage
	if len(cfg.EtcdServers) == 0 {
		cfg.EtcdServers = []string{"http://localhost:2379"}
	}
	genericConfig.RESTOptionsGetter = &simpleRESTOptionsGetter{
		etcdServers: cfg.EtcdServers,
		scheme:      Scheme,
		codecs:      Codecs,
	}

	// Set up OpenAPI
	genericConfig.Config.OpenAPIConfig = genericapiserver.DefaultOpenAPIConfig(nil, openapi.NewDefinitionNamer(Scheme))
	genericConfig.Config.OpenAPIConfig.Info.Title = "Example Generic Control Plane"
	genericConfig.Config.OpenAPIConfig.Info.Version = "v1alpha1"

	// Create kausality admission plugin
	kausalityPlugin := kausalityAdmission.NewKausalityAdmission(cfg.Client, cfg.Log, cfg.PolicyResolver)
	kausalityPlugin.SetScheme(Scheme)

	// Set up admission chain
	genericConfig.Config.AdmissionControl = admission.NewChainHandler(kausalityPlugin)

	// Create the server
	completedConfig := genericConfig.Complete()
	genericServer, err := completedConfig.New("example-apiserver", genericapiserver.NewEmptyDelegate())
	if err != nil {
		listener.Close()
		return nil, fmt.Errorf("failed to create generic server: %w", err)
	}

	// Install API group
	if err := installAPIGroup(genericServer, completedConfig.RESTOptionsGetter); err != nil {
		listener.Close()
		return nil, fmt.Errorf("failed to install API group: %w", err)
	}

	return &Server{
		GenericAPIServer: genericServer,
		log:              cfg.Log,
		listener:         listener,
	}, nil
}

// Run starts the API server.
func (s *Server) Run(ctx context.Context) error {
	s.log.Info("starting API server")
	return s.GenericAPIServer.PrepareRun().RunWithContext(ctx)
}

// installAPIGroup installs the example.kausality.io API group.
func installAPIGroup(s *genericapiserver.GenericAPIServer, restOptionsGetter generic.RESTOptionsGetter) error {
	// Create storage for Widget
	widgetStrategy := widget.NewStrategy(Scheme)
	widgetStore := &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &examplev1alpha1.Widget{} },
		NewListFunc:              func() runtime.Object { return &examplev1alpha1.WidgetList{} },
		DefaultQualifiedResource: examplev1alpha1.Resource("widgets"),
		SingularQualifiedResource: schema.GroupResource{
			Group:    examplev1alpha1.GroupVersion.Group,
			Resource: "widget",
		},
		CreateStrategy: widgetStrategy,
		UpdateStrategy: widgetStrategy,
		DeleteStrategy: widgetStrategy,
		TableConvertor: rest.NewDefaultTableConvertor(examplev1alpha1.Resource("widgets")),
	}
	widgetOpts, err := restOptionsGetter.GetRESTOptions(examplev1alpha1.Resource("widgets"), nil)
	if err != nil {
		return fmt.Errorf("failed to get REST options for widgets: %w", err)
	}
	if err := widgetStore.CompleteWithOptions(&generic.StoreOptions{
		RESTOptions: widgetOpts,
		AttrFunc:    widget.GetAttrs,
	}); err != nil {
		return fmt.Errorf("failed to complete widget store: %w", err)
	}

	// Create storage for WidgetSet
	widgetSetStrategy := widgetset.NewStrategy(Scheme)
	widgetSetStatusStrategy := widgetset.NewStatusStrategy(widgetSetStrategy)
	widgetSetStore := &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &examplev1alpha1.WidgetSet{} },
		NewListFunc:              func() runtime.Object { return &examplev1alpha1.WidgetSetList{} },
		DefaultQualifiedResource: examplev1alpha1.Resource("widgetsets"),
		SingularQualifiedResource: schema.GroupResource{
			Group:    examplev1alpha1.GroupVersion.Group,
			Resource: "widgetset",
		},
		CreateStrategy: widgetSetStrategy,
		UpdateStrategy: widgetSetStrategy,
		DeleteStrategy: widgetSetStrategy,
		TableConvertor: rest.NewDefaultTableConvertor(examplev1alpha1.Resource("widgetsets")),
	}
	widgetSetOpts, err := restOptionsGetter.GetRESTOptions(examplev1alpha1.Resource("widgetsets"), nil)
	if err != nil {
		return fmt.Errorf("failed to get REST options for widgetsets: %w", err)
	}
	if err := widgetSetStore.CompleteWithOptions(&generic.StoreOptions{
		RESTOptions: widgetSetOpts,
		AttrFunc:    widgetset.GetAttrs,
	}); err != nil {
		return fmt.Errorf("failed to complete widgetset store: %w", err)
	}

	// Create status subresource storage
	widgetSetStatusStore := *widgetSetStore
	widgetSetStatusStore.UpdateStrategy = widgetSetStatusStrategy

	// Build API group
	apiGroupInfo := genericapiserver.NewDefaultAPIGroupInfo(examplev1alpha1.GroupVersion.Group, Scheme, runtime.NewParameterCodec(Scheme), Codecs)
	apiGroupInfo.VersionedResourcesStorageMap[examplev1alpha1.GroupVersion.Version] = map[string]rest.Storage{
		"widgets":           widgetStore,
		"widgetsets":        widgetSetStore,
		"widgetsets/status": &widgetSetStatusStore,
	}

	return s.InstallAPIGroup(&apiGroupInfo)
}

// simpleRESTOptionsGetter provides REST options for storage.
type simpleRESTOptionsGetter struct {
	etcdServers []string
	scheme      *runtime.Scheme
	codecs      serializer.CodecFactory
}

func (g *simpleRESTOptionsGetter) GetRESTOptions(resource schema.GroupResource, example runtime.Object) (generic.RESTOptions, error) {
	storageConfig := &storagebackend.ConfigForResource{
		Config: storagebackend.Config{
			Type: storagebackend.StorageTypeETCD3,
			Transport: storagebackend.TransportConfig{
				ServerList: g.etcdServers,
			},
			Prefix: "/registry/example.kausality.io",
			Codec:  g.codecs.LegacyCodec(examplev1alpha1.GroupVersion),
		},
		GroupResource: resource,
	}
	return generic.RESTOptions{
		StorageConfig:           storageConfig,
		Decorator:               generic.UndecoratedStorage,
		EnableGarbageCollection: true,
		DeleteCollectionWorkers: 1,
		ResourcePrefix:          resource.Resource,
	}, nil
}

// allowAllAuthorizer allows all requests (no authz).
type allowAllAuthorizer struct{}

func (allowAllAuthorizer) Authorize(ctx context.Context, a authorizer.Attributes) (authorizer.Decision, string, error) {
	return authorizer.DecisionAllow, "", nil
}
