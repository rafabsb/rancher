package managementstored

import (
	"context"
	"net/http"
	"sync"

	"github.com/rancher/norman/store/crd"
	"github.com/rancher/norman/store/proxy"
	"github.com/rancher/norman/store/subtype"
	"github.com/rancher/norman/types"
	"github.com/rancher/rancher/pkg/api/customization/alert"
	"github.com/rancher/rancher/pkg/api/customization/app"
	"github.com/rancher/rancher/pkg/api/customization/authn"
	"github.com/rancher/rancher/pkg/api/customization/catalog"
	ccluster "github.com/rancher/rancher/pkg/api/customization/cluster"
	"github.com/rancher/rancher/pkg/api/customization/clusterregistrationtokens"
	"github.com/rancher/rancher/pkg/api/customization/logging"
	"github.com/rancher/rancher/pkg/api/customization/node"
	"github.com/rancher/rancher/pkg/api/customization/nodetemplate"
	"github.com/rancher/rancher/pkg/api/customization/pipeline"
	"github.com/rancher/rancher/pkg/api/customization/podsecuritypolicytemplate"
	projectaction "github.com/rancher/rancher/pkg/api/customization/project"
	"github.com/rancher/rancher/pkg/api/customization/setting"
	"github.com/rancher/rancher/pkg/api/store/cert"
	"github.com/rancher/rancher/pkg/api/store/cluster"
	nodeStore "github.com/rancher/rancher/pkg/api/store/node"
	"github.com/rancher/rancher/pkg/api/store/noopwatching"
	"github.com/rancher/rancher/pkg/api/store/preference"
	"github.com/rancher/rancher/pkg/api/store/scoped"
	"github.com/rancher/rancher/pkg/api/store/userscope"
	"github.com/rancher/rancher/pkg/auth/principals"
	"github.com/rancher/rancher/pkg/auth/providers"
	"github.com/rancher/rancher/pkg/clustermanager"
	"github.com/rancher/rancher/pkg/controllers/management/compose/common"
	"github.com/rancher/rancher/pkg/nodeconfig"
	managementschema "github.com/rancher/types/apis/management.cattle.io/v3/schema"
	projectschema "github.com/rancher/types/apis/project.cattle.io/v3/schema"
	"github.com/rancher/types/client/management/v3"
	projectclient "github.com/rancher/types/client/project/v3"
	"github.com/rancher/types/config"
)

func Setup(ctx context.Context, apiContext *config.ScaledContext, clusterManager *clustermanager.Manager,
	k8sProxy http.Handler) error {
	// Here we setup all types that will be stored in the Management cluster
	schemas := apiContext.Schemas

	wg := &sync.WaitGroup{}
	factory := &crd.Factory{ClientGetter: apiContext.ClientGetter}

	createCrd(ctx, wg, factory, schemas, &managementschema.Version,
		client.AuthConfigType,
		client.ClusterAlertType,
		client.ProjectAlertType,
		client.NotifierType,
		client.CatalogType,
		client.ClusterEventType,
		client.ClusterLoggingType,
		client.ClusterRegistrationTokenType,
		client.ClusterRoleTemplateBindingType,
		client.ClusterType,
		client.ClusterComposeConfigType,
		client.GlobalComposeConfigType,
		client.DynamicSchemaType,
		client.GlobalRoleBindingType,
		client.GlobalRoleType,
		client.GroupMemberType,
		client.GroupType,
		client.ListenConfigType,
		client.NodeType,
		client.NodePoolType,
		client.NodeDriverType,
		client.NodeTemplateType,
		client.PodSecurityPolicyTemplateType,
		client.PodSecurityPolicyTemplateProjectBindingType,
		client.PreferenceType,
		client.ProjectLoggingType,
		client.ProjectNetworkPolicyType,
		client.ProjectRoleTemplateBindingType,
		client.ProjectType,
		client.RoleTemplateType,
		client.SettingType,
		client.TemplateType,
		client.TemplateVersionType,
		client.TemplateContentType,
		client.ClusterPipelineType,
		client.PipelineType,
		client.PipelineExecutionType,
		client.PipelineExecutionLogType,
		client.SourceCodeCredentialType,
		client.SourceCodeRepositoryType,
		client.TokenType,
		client.UserType)
	createCrd(ctx, wg, factory, schemas, &projectschema.Version,
		projectclient.AppType, projectclient.AppRevisionType, projectclient.NamespaceComposeConfigType)

	wg.Wait()

	Clusters(schemas, apiContext, clusterManager, k8sProxy)
	Templates(schemas, apiContext)
	TemplateVersion(schemas, apiContext)
	User(schemas, apiContext)
	Catalog(schemas, apiContext)
	SecretTypes(ctx, schemas, apiContext)
	App(schemas, apiContext, clusterManager)
	Setting(schemas)
	Preference(schemas, apiContext)
	ClusterRegistrationTokens(schemas)
	NodeTemplates(schemas, apiContext)
	LoggingTypes(schemas)
	Alert(schemas, apiContext)
	Pipeline(schemas, apiContext)
	Project(schemas, apiContext)
	TemplateContent(schemas)
	PodSecurityPolicyTemplate(schemas, apiContext)

	if err := NodeTypes(schemas, apiContext); err != nil {
		return err
	}

	principals.Schema(ctx, apiContext, schemas)
	providers.SetupAuthConfig(ctx, apiContext, schemas)
	authn.SetUserStore(schemas.Schema(&managementschema.Version, client.UserType), apiContext)
	authn.SetRTBStore(ctx, schemas.Schema(&managementschema.Version, client.ClusterRoleTemplateBindingType), apiContext)
	authn.SetRTBStore(ctx, schemas.Schema(&managementschema.Version, client.ProjectRoleTemplateBindingType), apiContext)
	nodeStore.SetupStore(schemas.Schema(&managementschema.Version, client.NodeType))

	setupScopedTypes(schemas)

	return nil
}

func setupScopedTypes(schemas *types.Schemas) {
	for _, schema := range schemas.Schemas() {
		if schema.Scope != types.NamespaceScope || schema.Store == nil || schema.Store.Context() != config.ManagementStorageContext {
			continue
		}

		for _, key := range []string{"projectId", "clusterId"} {
			ns, ok := schema.ResourceFields["namespaceId"]
			if !ok {
				continue
			}

			if _, ok := schema.ResourceFields[key]; !ok {
				continue
			}

			schema.Store = scoped.NewScopedStore(key, schema.Store)
			ns.Required = false
			schema.ResourceFields["namespaceId"] = ns
			break
		}
	}
}

func Clusters(schemas *types.Schemas, managementContext *config.ScaledContext, clusterManager *clustermanager.Manager, k8sProxy http.Handler) {
	linkHandler := &ccluster.ShellLinkHandler{
		Proxy:          k8sProxy,
		ClusterManager: clusterManager,
	}
	handler := ccluster.ActionHandler{
		ClusterClient:  managementContext.Management.Clusters(""),
		UserMgr:        managementContext.UserManager,
		ClusterManager: clusterManager,
	}

	schema := schemas.Schema(&managementschema.Version, client.ClusterType)
	schema.Formatter = ccluster.Formatter
	schema.ActionHandler = handler.ClusterActionHandler
	schema.Store = &cluster.Store{
		Store:        schema.Store,
		ShellHandler: linkHandler.LinkHandler,
	}
}

func Templates(schemas *types.Schemas, managementContext *config.ScaledContext) {
	schema := schemas.Schema(&managementschema.Version, client.TemplateType)
	schema.Formatter = catalog.TemplateFormatter
	wrapper := catalog.TemplateWrapper{
		TemplateContentClient: managementContext.Management.TemplateContents(""),
	}
	schema.LinkHandler = wrapper.TemplateIconHandler
}

func TemplateVersion(schemas *types.Schemas, managementContext *config.ScaledContext) {
	schema := schemas.Schema(&managementschema.Version, client.TemplateVersionType)
	t := catalog.TemplateVerionFormatterWrapper{
		TemplateContentClient: managementContext.Management.TemplateContents(""),
	}
	schema.Formatter = t.TemplateVersionFormatter
	schema.LinkHandler = t.TemplateVersionReadmeHandler
	schema.Store = noopwatching.Wrap(schema.Store)
}

func TemplateContent(schemas *types.Schemas) {
	schema := schemas.Schema(&managementschema.Version, client.TemplateContentType)
	schema.Store = noopwatching.Wrap(schema.Store)
}

func Catalog(schemas *types.Schemas, managementContext *config.ScaledContext) {
	schema := schemas.Schema(&managementschema.Version, client.CatalogType)
	schema.Formatter = catalog.Formatter
	handler := catalog.ActionHandler{
		CatalogClient: managementContext.Management.Catalogs(""),
	}
	schema.ActionHandler = handler.RefreshActionHandler
	schema.CollectionFormatter = catalog.CollectionFormatter
}

func ClusterRegistrationTokens(schemas *types.Schemas) {
	schema := schemas.Schema(&managementschema.Version, client.ClusterRegistrationTokenType)
	schema.Store = &cluster.RegistrationTokenStore{
		Store: schema.Store,
	}
	schema.Formatter = clusterregistrationtokens.Formatter
}

func NodeTemplates(schemas *types.Schemas, management *config.ScaledContext) {
	schema := schemas.Schema(&managementschema.Version, client.NodeTemplateType)
	schema.Store = userscope.NewStore(management.Core.Namespaces(""), schema.Store)
	schema.Validator = nodetemplate.Validator
}

func SecretTypes(ctx context.Context, schemas *types.Schemas, management *config.ScaledContext) {
	secretSchema := schemas.Schema(&projectschema.Version, projectclient.SecretType)
	secretSchema.Store = proxy.NewProxyStore(ctx, management.ClientGetter,
		config.ManagementStorageContext,
		[]string{"api"},
		"",
		"v1",
		"Secret",
		"secrets")

	for _, subSchema := range schemas.SchemasForVersion(projectschema.Version) {
		if subSchema.BaseType == projectclient.SecretType && subSchema.ID != projectclient.SecretType {
			if subSchema.CanList(nil) == nil {
				subSchema.Store = subtype.NewSubTypeStore(subSchema.ID, secretSchema.Store)
			}
		}
	}

	secretSchema = schemas.Schema(&projectschema.Version, projectclient.CertificateType)
	secretSchema.Store = cert.Wrap(secretSchema.Store)
}

func User(schemas *types.Schemas, management *config.ScaledContext) {
	schema := schemas.Schema(&managementschema.Version, client.UserType)
	schema.Formatter = authn.UserFormatter
	schema.CollectionFormatter = authn.CollectionFormatter
	handler := &authn.Handler{
		UserClient: management.Management.Users(""),
	}
	schema.ActionHandler = handler.Actions
}

func Preference(schemas *types.Schemas, management *config.ScaledContext) {
	schema := schemas.Schema(&managementschema.Version, client.PreferenceType)
	schema.Store = preference.NewStore(management.Core.Namespaces(""), schema.Store)
}

func NodeTypes(schemas *types.Schemas, management *config.ScaledContext) error {
	secretStore, err := nodeconfig.NewStore(management.Core.Namespaces(""), management.K8sClient.CoreV1())
	if err != nil {
		return err
	}

	schema := schemas.Schema(&managementschema.Version, client.NodeDriverType)
	machineDriverHandlers := &node.DriverHandlers{
		NodeDriverClient: management.Management.NodeDrivers(""),
	}
	schema.Formatter = machineDriverHandlers.Formatter
	schema.ActionHandler = machineDriverHandlers.ActionHandler

	machineHandler := &node.DriverHandler{
		SecretStore: secretStore,
	}

	schema = schemas.Schema(&managementschema.Version, client.NodeType)
	schema.Formatter = node.Formatter
	schema.LinkHandler = machineHandler.LinkHandler

	return nil
}

func App(schemas *types.Schemas, management *config.ScaledContext, kubeConfigGetter common.KubeConfigGetter) {
	schema := schemas.Schema(&projectschema.Version, projectclient.AppType)
	wrapper := app.Wrapper{
		Clusters:              management.Management.Clusters(""),
		KubeConfigGetter:      kubeConfigGetter,
		TemplateContentClient: management.Management.TemplateContents(""),
		AppGetter:             management.Project,
	}
	schema.Formatter = app.Formatter
	schema.ActionHandler = wrapper.ActionHandler
	schema.LinkHandler = wrapper.LinkHandler
}

func Setting(schemas *types.Schemas) {
	schema := schemas.Schema(&managementschema.Version, client.SettingType)
	schema.Formatter = setting.Formatter
}

func LoggingTypes(schemas *types.Schemas) {
	schema := schemas.Schema(&managementschema.Version, client.ClusterLoggingType)
	schema.Validator = logging.ClusterLoggingValidator

	schema = schemas.Schema(&managementschema.Version, client.ProjectLoggingType)
	schema.Validator = logging.ProjectLoggingValidator
}

func Alert(schemas *types.Schemas, management *config.ScaledContext) {
	handler := &alert.Handler{
		ProjectAlerts: management.Management.ProjectAlerts(""),
		ClusterAlerts: management.Management.ClusterAlerts(""),
		Notifiers:     management.Management.Notifiers(""),
	}

	schema := schemas.Schema(&managementschema.Version, client.ClusterAlertType)
	schema.Formatter = alert.Formatter
	schema.ActionHandler = handler.ClusterActionHandler

	schema = schemas.Schema(&managementschema.Version, client.ProjectAlertType)
	schema.Formatter = alert.Formatter
	schema.ActionHandler = handler.ProjectActionHandler

	schema = schemas.Schema(&managementschema.Version, client.NotifierType)
	schema.CollectionFormatter = alert.NotifierCollectionFormatter
	schema.Formatter = alert.NotifierFormatter
	schema.ActionHandler = handler.NotifierActionHandler
}

func Pipeline(schemas *types.Schemas, management *config.ScaledContext) {

	clusterPipelineHandler := &pipeline.ClusterPipelineHandler{
		ClusterPipelines:           management.Management.ClusterPipelines(""),
		ClusterPipelineLister:      management.Management.ClusterPipelines("").Controller().Lister(),
		SourceCodeCredentials:      management.Management.SourceCodeCredentials(""),
		SourceCodeCredentialLister: management.Management.SourceCodeCredentials("").Controller().Lister(),
		SourceCodeRepositories:     management.Management.SourceCodeRepositories(""),
		SourceCodeRepositoryLister: management.Management.SourceCodeRepositories("").Controller().Lister(),
		Secrets:                    management.Core.Secrets(""),
		SecretLister:               management.Core.Secrets("").Controller().Lister(),
		AuthConfigs:                management.Management.AuthConfigs(""),
	}
	schema := schemas.Schema(&managementschema.Version, client.ClusterPipelineType)
	schema.Formatter = pipeline.ClusterPipelineFormatter
	schema.ActionHandler = clusterPipelineHandler.ActionHandler
	schema.LinkHandler = clusterPipelineHandler.LinkHandler
	pipelineHandler := &pipeline.Handler{
		Pipelines:          management.Management.Pipelines(""),
		PipelineLister:     management.Management.Pipelines("").Controller().Lister(),
		PipelineExecutions: management.Management.PipelineExecutions(""),
	}
	schema = schemas.Schema(&managementschema.Version, client.PipelineType)
	schema.Formatter = pipeline.Formatter
	schema.ActionHandler = pipelineHandler.ActionHandler
	schema.CreateHandler = pipelineHandler.CreateHandler
	schema.UpdateHandler = pipelineHandler.UpdateHandler
	schema.LinkHandler = pipelineHandler.LinkHandler
	schema.Validator = pipeline.Validator

	pipelineExecutionHandler := &pipeline.ExecutionHandler{}
	schema = schemas.Schema(&managementschema.Version, client.PipelineExecutionType)
	schema.Formatter = pipelineExecutionHandler.ExecutionFormatter
	schema.LinkHandler = pipelineExecutionHandler.LinkHandler
	schema.ActionHandler = pipelineExecutionHandler.ActionHandler

	sourceCodeCredentialHandler := &pipeline.SourceCodeCredentialHandler{
		ClusterPipelineLister:      management.Management.ClusterPipelines("").Controller().Lister(),
		SourceCodeCredentials:      management.Management.SourceCodeCredentials(""),
		SourceCodeCredentialLister: management.Management.SourceCodeCredentials("").Controller().Lister(),
		SourceCodeRepositories:     management.Management.SourceCodeRepositories(""),
		SourceCodeRepositoryLister: management.Management.SourceCodeRepositories("").Controller().Lister(),
	}
	schema = schemas.Schema(&managementschema.Version, client.SourceCodeCredentialType)
	schema.Formatter = pipeline.SourceCodeCredentialFormatter
	schema.ActionHandler = sourceCodeCredentialHandler.ActionHandler
	schema.LinkHandler = sourceCodeCredentialHandler.LinkHandler
	schema.Store = userscope.NewStore(management.Core.Namespaces(""), schema.Store)

	sourceCodeRepositoryHandler := &pipeline.SourceCodeRepositoryHandler{
		SourceCodeCredentialLister: management.Management.SourceCodeCredentials("").Controller().Lister(),
		SourceCodeRepositoryLister: management.Management.SourceCodeRepositories("").Controller().Lister(),
		ClusterPipelineLister:      management.Management.ClusterPipelines("").Controller().Lister(),
	}
	schema = schemas.Schema(&managementschema.Version, client.SourceCodeRepositoryType)
	schema.Store = userscope.NewStore(management.Core.Namespaces(""), schema.Store)
	schema.Formatter = pipeline.SourceCodeRepositoryFormatter
	schema.LinkHandler = sourceCodeRepositoryHandler.LinkHandler
}

func Project(schemas *types.Schemas, management *config.ScaledContext) {
	schema := schemas.Schema(&managementschema.Version, client.ProjectType)
	schema.Formatter = projectaction.Formatter
	handler := &projectaction.Handler{
		Projects:       management.Management.Projects(""),
		ProjectLister:  management.Management.Projects("").Controller().Lister(),
		UserMgr:        management.UserManager,
		ClusterManager: management.ClientGetter.(*clustermanager.Manager),
	}
	schema.ActionHandler = handler.Actions
}

func PodSecurityPolicyTemplate(schemas *types.Schemas, management *config.ScaledContext) {

	schema := schemas.Schema(&managementschema.Version, client.PodSecurityPolicyTemplateType)
	schema.Formatter = podsecuritypolicytemplate.NewFormatter(management)
	schema.Store = &podsecuritypolicytemplate.Store{
		Store: schema.Store,
	}
}
