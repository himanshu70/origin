package requestlimit

import (
	"fmt"
	"io"

	"github.com/golang/glog"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/authentication/serviceaccount"
	"k8s.io/client-go/rest"

	"github.com/openshift/api/project"
	usertypedclient "github.com/openshift/client-go/user/clientset/versioned/typed/user/v1"
	"github.com/openshift/origin/pkg/api/legacy"
	oadmission "github.com/openshift/origin/pkg/cmd/server/admission"
	configlatest "github.com/openshift/origin/pkg/cmd/server/apis/config/latest"
	projectapi "github.com/openshift/origin/pkg/project/apis/project"
	requestlimitapi "github.com/openshift/origin/pkg/project/apiserver/admission/apis/requestlimit"
	requestlimitapivalidation "github.com/openshift/origin/pkg/project/apiserver/admission/apis/requestlimit/validation"
	projectcache "github.com/openshift/origin/pkg/project/cache"
	uservalidation "github.com/openshift/origin/pkg/user/apis/user/validation"
)

// allowedTerminatingProjects is the number of projects that are owned by a user, are in terminating state,
// and do not count towards the user's limit.
const allowedTerminatingProjects = 2

func Register(plugins *admission.Plugins) {
	plugins.Register("ProjectRequestLimit",
		func(config io.Reader) (admission.Interface, error) {
			pluginConfig, err := readConfig(config)
			if err != nil {
				return nil, err
			}
			if pluginConfig == nil {
				glog.Infof("Admission plugin %q is not configured so it will be disabled.", "ProjectRequestLimit")
				return nil, nil
			}
			return NewProjectRequestLimit(pluginConfig)
		})
}

func readConfig(reader io.Reader) (*requestlimitapi.ProjectRequestLimitConfig, error) {
	obj, err := configlatest.ReadYAML(reader)
	if err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, nil
	}
	config, ok := obj.(*requestlimitapi.ProjectRequestLimitConfig)
	if !ok {
		return nil, fmt.Errorf("unexpected config object: %#v", obj)
	}
	errs := requestlimitapivalidation.ValidateProjectRequestLimitConfig(config)
	if len(errs) > 0 {
		return nil, errs.ToAggregate()
	}
	return config, nil
}

type projectRequestLimit struct {
	*admission.Handler
	userClient usertypedclient.UsersGetter
	config     *requestlimitapi.ProjectRequestLimitConfig
	cache      *projectcache.ProjectCache
}

// ensure that the required Openshift admission interfaces are implemented
var _ = oadmission.WantsProjectCache(&projectRequestLimit{})
var _ = oadmission.WantsRESTClientConfig(&projectRequestLimit{})
var _ = admission.ValidationInterface(&projectRequestLimit{})

// Admit ensures that only a configured number of projects can be requested by a particular user.
func (o *projectRequestLimit) Validate(a admission.Attributes) (err error) {
	if o.config == nil {
		return nil
	}
	switch a.GetResource().GroupResource() {
	case project.Resource("projectrequests"), legacy.Resource("projectrequests"):
	default:
		return nil
	}
	if _, isProjectRequest := a.GetObject().(*projectapi.ProjectRequest); !isProjectRequest {
		return nil
	}
	userName := a.GetUserInfo().GetName()
	projectCount, err := o.projectCountByRequester(userName)
	if err != nil {
		return err
	}
	maxProjects, hasLimit, err := o.maxProjectsByRequester(userName)
	if err != nil {
		return err
	}
	if hasLimit && projectCount >= maxProjects {
		return admission.NewForbidden(a, fmt.Errorf("user %s cannot create more than %d project(s).", userName, maxProjects))
	}
	return nil
}

// maxProjectsByRequester returns the maximum number of projects allowed for a given user, whether a limit exists, and an error
// if an error occurred. If a limit doesn't exist, the maximum number should be ignored.
func (o *projectRequestLimit) maxProjectsByRequester(userName string) (int, bool, error) {
	// service accounts have a different ruleset, check them
	if _, _, err := serviceaccount.SplitUsername(userName); err == nil {
		if o.config.MaxProjectsForServiceAccounts == nil {
			return 0, false, nil
		}

		return *o.config.MaxProjectsForServiceAccounts, true, nil
	}

	// if we aren't a valid username, we came in as cert user for certain, use our cert user rules
	if reasons := uservalidation.ValidateUserName(userName, false); len(reasons) != 0 {
		if o.config.MaxProjectsForSystemUsers == nil {
			return 0, false, nil
		}

		return *o.config.MaxProjectsForSystemUsers, true, nil
	}

	// prevent a user lookup if no limits are configured
	if len(o.config.Limits) == 0 {
		return 0, false, nil
	}

	user, err := o.userClient.Users().Get(userName, metav1.GetOptions{})
	if err != nil {
		return 0, false, err
	}
	userLabels := labels.Set(user.Labels)

	for _, limit := range o.config.Limits {
		selector := labels.Set(limit.Selector).AsSelector()
		if selector.Matches(userLabels) {
			if limit.MaxProjects == nil {
				return 0, false, nil
			}
			return *limit.MaxProjects, true, nil
		}
	}
	return 0, false, nil
}

func (o *projectRequestLimit) projectCountByRequester(userName string) (int, error) {
	namespaces, err := o.cache.Store.ByIndex("requester", userName)
	if err != nil {
		return 0, err
	}

	terminatingCount := 0
	for _, obj := range namespaces {
		ns, ok := obj.(*corev1.Namespace)
		if !ok {
			return 0, fmt.Errorf("object in cache is not a namespace: %#v", obj)
		}
		if ns.Status.Phase == corev1.NamespaceTerminating {
			terminatingCount++
		}
	}
	count := len(namespaces)
	if terminatingCount > allowedTerminatingProjects {
		count -= allowedTerminatingProjects
	} else {
		count -= terminatingCount
	}
	return count, nil
}

func (o *projectRequestLimit) SetRESTClientConfig(restClientConfig rest.Config) {
	var err error
	o.userClient, err = usertypedclient.NewForConfig(&restClientConfig)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
}

func (o *projectRequestLimit) SetProjectCache(cache *projectcache.ProjectCache) {
	o.cache = cache
}

func (o *projectRequestLimit) ValidateInitialization() error {
	if o.userClient == nil {
		return fmt.Errorf("ProjectRequestLimit plugin requires an Openshift client")
	}
	if o.cache == nil {
		return fmt.Errorf("ProjectRequestLimit plugin requires a project cache")
	}
	return nil
}

func NewProjectRequestLimit(config *requestlimitapi.ProjectRequestLimitConfig) (admission.Interface, error) {
	return &projectRequestLimit{
		config:  config,
		Handler: admission.NewHandler(admission.Create),
	}, nil
}
