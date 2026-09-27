package httpapi

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Amaan729/ForgeRail/api"
)

type openAPISpec struct {
	Paths      map[string]map[string]any `yaml:"paths"`
	Components struct {
		Schemas map[string]struct {
			Required   []string       `yaml:"required"`
			Properties map[string]any `yaml:"properties"`
		} `yaml:"schemas"`
	} `yaml:"components"`
}

func loadSpec(t *testing.T) openAPISpec {
	t.Helper()
	var spec openAPISpec
	if err := yaml.Unmarshal(api.OpenAPI, &spec); err != nil {
		t.Fatalf("openapi.yaml does not parse: %v", err)
	}
	return spec
}

// TestRoutesMatchSpec fails if a route is added to the handler but not the
// spec, or the other way round.
func TestRoutesMatchSpec(t *testing.T) {
	spec := loadSpec(t)
	inSpec := map[string]bool{}
	for path, ops := range spec.Paths {
		for method := range ops {
			switch method {
			case "get", "post", "put", "patch", "delete":
				inSpec[strings.ToUpper(method)+" "+path] = true
			}
		}
	}
	inCode := map[string]bool{}
	for _, r := range Routes {
		inCode[r.Method+" "+r.Path] = true
	}
	for k := range inCode {
		if !inSpec[k] {
			t.Errorf("%s is served but missing from openapi.yaml", k)
		}
	}
	for k := range inSpec {
		if !inCode[k] {
			t.Errorf("%s is in openapi.yaml but not served", k)
		}
	}
}

// TestSchemasMatchJSON compares struct json tags with schema properties.
func TestSchemasMatchJSON(t *testing.T) {
	spec := loadSpec(t)
	cases := map[string]any{
		"Account":               accountJSON{},
		"Transfer":              transferJSON{},
		"Entry":                 entryJSON{},
		"CreateTransferRequest": createTransferJSON{},
	}
	for name, v := range cases {
		schema, ok := spec.Components.Schemas[name]
		if !ok {
			t.Errorf("schema %s missing from spec", name)
			continue
		}
		var props []string
		for p := range schema.Properties {
			props = append(props, p)
		}
		sort.Strings(props)
		fields := jsonFields(v)
		if !reflect.DeepEqual(props, fields) {
			t.Errorf("%s: spec properties %v != json fields %v", name, props, fields)
		}
		for _, req := range schema.Required {
			if _, ok := schema.Properties[req]; !ok {
				t.Errorf("%s: required field %q is not a property", name, req)
			}
		}
	}
}

func jsonFields(v any) []string {
	var out []string
	rt := reflect.TypeOf(v)
	for i := 0; i < rt.NumField(); i++ {
		name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
