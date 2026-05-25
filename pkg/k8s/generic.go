package k8s

// Generic read-only resource fetch backing the agent's get_resource tool.
//
// The typed tools (pods, deployments, PVCs, ...) cover the kinds we diagnose
// deeply. This is the catch-all: any other kind (StatefulSet, DaemonSet, Job,
// PersistentVolume, Service, Ingress, HPA, Node, even CRDs) is reachable via
// the dynamic client + a discovery-backed RESTMapper that turns a human kind
// name into the GroupVersionResource the API server actually serves.
//
// Read-only by construction: only Get and List are issued here. Output is
// sanitized before it leaves the cluster boundary — Secret values are redacted
// and serialization noise (managedFields, last-applied) is dropped so it does
// not pollute the LLM context or risk leaking blobs.

import (
	"context"
	"fmt"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	memcache "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/yaml"
)

// genericClients is lazily built on first get_resource call. Discovery does a
// round trip to enumerate the API surface, so we don't pay it at startup —
// only sessions that actually touch an exotic kind incur the cost, once.
type genericClients struct {
	dyn    dynamic.Interface
	mapper meta.RESTMapper
}

var (
	genericOnce sync.Once
	genericVal  *genericClients
	genericErr  error
)

func (c *Client) generic() (*genericClients, error) {
	genericOnce.Do(func() {
		dyn, err := dynamic.NewForConfig(c.restConfig)
		if err != nil {
			genericErr = fmt.Errorf("could not build dynamic client: %w", err)
			return
		}
		dc, err := discovery.NewDiscoveryClientForConfig(c.restConfig)
		if err != nil {
			genericErr = fmt.Errorf("could not build discovery client: %w", err)
			return
		}
		// Memory-cached discovery so repeated get_resource calls in one session
		// don't re-hit the discovery endpoint per call.
		mapper := restmapper.NewDeferredDiscoveryRESTMapper(memcache.NewMemCacheClient(dc))
		genericVal = &genericClients{dyn: dyn, mapper: mapper}
	})
	return genericVal, genericErr
}

// GetResource fetches one resource (or, if name is empty, lists the kind in the
// namespace) and returns sanitized YAML. kind is matched leniently: singular,
// plural, or shortname ("sts", "ds", "pv") all resolve via the RESTMapper.
// namespace is ignored for cluster-scoped kinds (Node, PersistentVolume, ...).
func (c *Client) GetResource(ctx context.Context, kind, namespace, name string) (string, error) {
	g, err := c.generic()
	if err != nil {
		return "", err
	}

	mapping, err := c.resolveKind(g, kind)
	if err != nil {
		return "", err
	}

	ri := dynamicResourceInterface(g, mapping, namespace)

	if name == "" {
		list, err := ri.List(ctx, metav1.ListOptions{})
		if err != nil {
			return "", fmt.Errorf("listing %s: %w", mapping.Resource.Resource, err)
		}
		for i := range list.Items {
			sanitize(&list.Items[i])
		}
		return marshalYAML(list)
	}

	obj, err := ri.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Errorf("%s %q not found in namespace %q", mapping.Resource.Resource, name, namespace)
		}
		return "", fmt.Errorf("getting %s %q: %w", mapping.Resource.Resource, name, err)
	}
	sanitize(obj)
	return marshalYAML(obj)
}

// resolveKind turns a free-form kind string into a REST mapping. It tries the
// RESTMapper's kind resolution first, then falls back to treating the input as
// a resource name/shortname so "sts"/"statefulsets"/"StatefulSet" all work.
func (c *Client) resolveKind(g *genericClients, kind string) (*meta.RESTMapping, error) {
	k := strings.TrimSpace(kind)
	if k == "" {
		return nil, fmt.Errorf("kind is required")
	}

	// Resolve to a GVR first — ResourceFor accepts plural ("statefulsets"),
	// singular ("statefulset"), and shortnames ("sts") via the discovery data.
	gvr, err := g.mapper.ResourceFor(schema.GroupVersionResource{Resource: strings.ToLower(k)})
	if err != nil {
		return nil, fmt.Errorf("unknown kind %q (the cluster does not serve it, or it needs a group qualifier)", kind)
	}
	// GVR -> GVK -> REST mapping (scope + canonical resource).
	gvk, err := g.mapper.KindFor(gvr)
	if err != nil {
		return nil, fmt.Errorf("could not resolve kind %q: %w", kind, err)
	}
	m, err := g.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, fmt.Errorf("could not map kind %q: %w", kind, err)
	}
	return m, nil
}

func dynamicResourceInterface(g *genericClients, mapping *meta.RESTMapping, namespace string) dynamic.ResourceInterface {
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		if namespace == "" {
			namespace = "default"
		}
		return g.dyn.Resource(mapping.Resource).Namespace(namespace)
	}
	return g.dyn.Resource(mapping.Resource)
}

// sanitize strips serialization noise and redacts secret material so neither
// leaks into the LLM context. Mirrors the redaction promise the typed
// describe_pod tool already makes about env vars sourced from Secrets.
func sanitize(o *unstructured.Unstructured) {
	unstructured.RemoveNestedField(o.Object, "metadata", "managedFields")
	unstructured.RemoveNestedField(o.Object, "metadata", "annotations",
		"kubectl.kubernetes.io/last-applied-configuration")

	if o.GetKind() == "Secret" {
		if _, ok, _ := unstructured.NestedMap(o.Object, "data"); ok {
			redactStringMap(o.Object, "data")
		}
		if _, ok, _ := unstructured.NestedMap(o.Object, "stringData"); ok {
			redactStringMap(o.Object, "stringData")
		}
	}
}

func redactStringMap(obj map[string]any, field string) {
	m, ok, _ := unstructured.NestedMap(obj, field)
	if !ok {
		return
	}
	for k := range m {
		m[k] = "**REDACTED**"
	}
	_ = unstructured.SetNestedMap(obj, m, field)
}

func marshalYAML(obj any) (string, error) {
	var raw any
	switch v := obj.(type) {
	case *unstructured.Unstructured:
		raw = v.Object
	case *unstructured.UnstructuredList:
		raw = v.Object
	default:
		raw = obj
	}
	b, err := yaml.Marshal(raw)
	if err != nil {
		return "", fmt.Errorf("rendering resource as YAML: %w", err)
	}
	return string(b), nil
}
