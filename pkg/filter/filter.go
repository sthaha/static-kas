package filter

import (
	"fmt"
	"net/http"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type Filter func(*unstructured.UnstructuredList) (*unstructured.UnstructuredList, error)

func FromRequest(r *http.Request) []Filter {
	return []Filter{filterForFieldSelector(r.URL.Query()["fieldSelector"])}
}
func filterForFieldSelector(value []string) Filter {
	selectorMap := make(map[string]string, len(value))
	return func(in *unstructured.UnstructuredList) (*unstructured.UnstructuredList, error) {
		if len(value) == 0 {
			return in, nil
		}
		var sanitizedValues []string
		for _, item := range value {
			sanitizedValues = append(sanitizedValues, strings.Split(item, ",")...)
		}
		for _, entry := range sanitizedValues {
			split := strings.Split(entry, "=")
			if len(split) != 2 {
				return nil, fmt.Errorf("field selector expression %s split by = doesn't yield exactly two results", entry)
			}
			selectorMap[split[0]] = split[1]
		}

		result := &unstructured.UnstructuredList{}
		result.SetGroupVersionKind(in.GroupVersionKind())

		for _, item := range in.Items {
			matches := true
			for k, v := range selectorMap {
				if value, _, _ := unstructured.NestedString(item.Object, strings.Split(k, ".")...); value != v {
					matches = false
					break
				}
			}
			if matches {
				result.Items = append(result.Items, *item.DeepCopy())
			}
		}

		return result, nil
	}
}
