package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gorilla/mux"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/felixge/httpsnoop"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/alvaroaleman/static-kas/pkg/filter"
	"github.com/alvaroaleman/static-kas/pkg/response"
	"github.com/alvaroaleman/static-kas/pkg/transform"
)

type options struct {
	baseDir string
}

func main() {

	o := options{}
	flag.StringVar(&o.baseDir, "base-dir", "", "The basedir of the cluster dump")
	flag.Parse()

	lCfg := zap.NewProductionConfig()
	lCfg.EncoderConfig.EncodeTime = zapcore.RFC3339TimeEncoder
	l, err := lCfg.Build()
	if err != nil {
		fmt.Printf("failed to construct logger: %v\n", err)
		os.Exit(1)
	}
	defer l.Sync()

	if o.baseDir == "" {
		l.Fatal("--base-dir is mandatory")
	}

	l.Info("Discovering api resources")
	groupResourceListMap, groupResourceMap, crdMap, err := discover(l, o.baseDir)
	if err != nil {
		l.Fatal("failed to discover apis", zap.Error(err))
	}
	groupSerializedResourceListMap, err := serializeAPIResourceList(groupResourceListMap)
	if err != nil {
		l.Fatal("failed to serialize apiresources", zap.Error(err))
	}
	groupList, err := apiGroupList(groupResourceListMap)
	if err != nil {
		l.Fatal("failed to construct api group list", zap.Error(err))
	}
	serializedGroupList, err := json.Marshal(groupList)
	if err != nil {
		l.Fatal("failed to serialize api group list", zap.Error(err))
	}
	allNamespaces := &unstructured.UnstructuredList{}
	allNamespaces.SetAPIVersion("v1")
	allNamespaces.SetKind("List")
	namespacePath := filepath.Join(o.baseDir, "namespaces")
	namespacesDirEntries, err := os.ReadDir(namespacePath)
	if err != nil {
		l.Fatal("failed to read namespaces folder", zap.String("path", namespacePath), zap.Error(err))
	}
	for _, entry := range namespacesDirEntries {
		ns := unstructured.Unstructured{}
		ns.SetAPIVersion("v1")
		ns.SetKind("Namespace")
		ns.SetName(entry.Name())
		allNamespaces.Items = append(allNamespaces.Items, ns)
	}
	l.Info("Finished discovering api resources")

	tableTransform := transform.NewTableTransformMap(crdMap)

	router := mux.NewRouter()
	router.Use(loggingMiddleware(l))
	router.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{}`))
	})
	router.HandleFunc("/api", func(w http.ResponseWriter, _ *http.Request) {
		d := metav1.APIVersions{TypeMeta: metav1.TypeMeta{Kind: "APIVersions"}, Versions: []string{"v1"}}
		serializeAndWrite(l, w, d)
	}).Methods(http.MethodGet)
	router.HandleFunc("/api/v1", func(w http.ResponseWriter, _ *http.Request) {
		w.Write(groupSerializedResourceListMap["v1"])
	}).Methods(http.MethodGet)
	router.HandleFunc("/api/v1/namespaces/{namespace}/{resource}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		l := l.With(zap.String("path", r.URL.Path))
		path := path.Join(o.baseDir, "namespaces", vars["namespace"], "core")
		var transformFunc transform.TransformFunc
		if acceptsTable(r) {
			transformFunc = tableTransform(transformKey(vars, transform.VerbList), tableVersion(r))
		}
		if err := response.NewListResponse(r, w, path, vars["resource"], transformFunc, nil, filter.FromRequest(r)...); err != nil {
			l.Error("failed to respond", zap.Error(err))
		}
	}).Methods(http.MethodGet)
	router.HandleFunc("/api/v1/namespaces/{namespace}/{resource}/{name}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		l := l.With(zap.String("path", r.URL.Path))
		var transformFunc transform.TransformFunc
		if acceptsTable(r) {
			transformFunc = tableTransform(transformKey(vars, transform.VerbGet), tableVersion(r))
		}
		path := path.Join(o.baseDir, "namespaces", vars["namespace"], "core")
		if err := response.NewGetResponse(r, w, path, vars["resource"], vars["name"], nil, transformFunc); err != nil {
			l.Error("failed to respond", zap.Error(err))
		}
	}).Methods(http.MethodGet)
	router.HandleFunc("/api/v1/namespaces/{namespace}/pods/{name}/log", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		containerName := r.URL.Query().Get("container")
		fileName := "current.log"
		hypershiftSuffix := ".log"
		if r.URL.Query().Get("previous") == "true" {
			fileName = "previous.log"
			hypershiftSuffix = "-previous.log"
		}
		paths := []string{
			path.Join(o.baseDir, "namespaces", vars["namespace"], "pods", vars["name"], containerName, containerName, "logs", fileName),
			path.Join(o.baseDir, "namespaces", vars["namespace"], "core", "pods", "logs", vars["name"]+"-"+containerName+hypershiftSuffix),
		}
		f, err := openFirstFound(paths)
		if err != nil {
			w.WriteHeader(404)
			w.Write([]byte(fmt.Sprintf("failed to open one of %v: %v", paths, err)))
			return
		}
		defer f.Close()

		if tailRaw := r.URL.Query().Get("tailLines"); tailRaw != "" {
			tailLines, err := strconv.Atoi(tailRaw)
			if err != nil {
				http.Error(w, "tailLines query arg must be an integer", http.StatusBadRequest)
				return
			}
			lines, err := tailFile(f, tailLines)
			if err != nil {
				http.Error(w, fmt.Sprintf("failed to read log: %v", err), http.StatusInternalServerError)
				return
			}
			w.Write(lines)
		} else {
			io.Copy(w, f)
		}

		// Block so the client doesn't get an EOF error
		if r.URL.Query().Get("follow") == "true" {
			// We have to force a flush first, because golang buffers responses until the handler returns or the buffer is filled.
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done()
		}
	}).Methods(http.MethodGet)
	router.HandleFunc("/api/v1/{resource}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		l := l.With(zap.String("path", r.URL.Path))
		var transformFunc transform.TransformFunc
		if acceptsTable(r) {
			transformFunc = tableTransform(transformKey(vars, transform.VerbList), tableVersion(r))
		}
		if groupResourceMap[groupVersionResource{groupVersion: "v1", resource: vars["resource"]}].Namespaced {
			if err := response.NewCrossNamespaceListResponse(r, w, filepath.Join(o.baseDir, "namespaces"), "core", vars["resource"], transformFunc); err != nil {
				l.Error("failed to respond", zap.Error(err))
			}
			return
		}
		path := path.Join(o.baseDir, "cluster-scoped-resources", "core")
		// Special snowflake, they are not being dumped by must-gather
		if vars["resource"] == "namespaces" {
			if err := response.NewListResponse(r, w, path, vars["resource"], transformFunc, allNamespaces, filter.FromRequest(r)...); err != nil {
				l.Error("failed to respond", zap.Error(err))
			}
			return
		}
		if err := response.NewListResponse(r, w, path, vars["resource"], transformFunc, nil, filter.FromRequest(r)...); err != nil {
			l.Error("failed to respond", zap.Error(err))
		}
	}).Methods(http.MethodGet)
	router.HandleFunc("/api/v1/{resource}/{name}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		l := l.With(zap.String("path", r.URL.Path))
		var transformFunc transform.TransformFunc
		if acceptsTable(r) {
			transformFunc = tableTransform(transformKey(vars, transform.VerbList), tableVersion(r))
		}
		path := path.Join(o.baseDir, "cluster-scoped-resources", "core")
		if vars["resource"] == "namespaces" {
			if err := response.NewGetResponse(r, w, path, vars["resource"], vars["name"], findByName(allNamespaces, vars["name"]), transformFunc); err != nil {
				l.Error("failed to respond", zap.Error(err))
			}
			return
		}
		if err := response.NewGetResponse(r, w, path, vars["resource"], vars["name"], nil, transformFunc); err != nil {
			l.Error("failed to respond", zap.Error(err))
		}
	}).Methods(http.MethodGet)
	router.HandleFunc("/apis", func(w http.ResponseWriter, _ *http.Request) {
		w.Write(serializedGroupList)
	}).Methods(http.MethodGet)
	for groupVersion := range groupSerializedResourceListMap {
		groupVersion := groupVersion
		router.HandleFunc("/apis/"+groupVersion, func(w http.ResponseWriter, _ *http.Request) {
			w.Write(groupSerializedResourceListMap[groupVersion])
		}).Methods(http.MethodGet)
	}
	router.HandleFunc("/apis/{group}/{version}/namespaces/{namespace}/{resource}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		l := l.With(zap.String("path", r.URL.Path))
		var transformFunc transform.TransformFunc
		if acceptsTable(r) {
			transformFunc = tableTransform(transformKey(vars, transform.VerbList), tableVersion(r))
		}
		path := path.Join(o.baseDir, "namespaces", vars["namespace"], vars["group"])
		if err := response.NewListResponse(r, w, path, vars["resource"], transformFunc, nil, filter.FromRequest(r)...); err != nil {
			l.Error("failed to respond", zap.Error(err))
		}
	}).Methods(http.MethodGet)
	router.HandleFunc("/apis/{group}/{version}/namespaces/{namespace}/{resource}/{name}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		l := l.With(zap.String("path", r.URL.Path))
		var transformFunc transform.TransformFunc
		if acceptsTable(r) {
			transformFunc = tableTransform(transformKey(vars, transform.VerbGet), tableVersion(r))
		}
		path := path.Join(o.baseDir, "namespaces", vars["namespace"], vars["group"])
		if err := response.NewGetResponse(r, w, path, vars["resource"], vars["name"], nil, transformFunc); err != nil {
			l.Error("failed to respond", zap.Error(err))
		}
	}).Methods(http.MethodGet)
	router.HandleFunc("/apis/authorization.k8s.io/{version}/selfsubjectaccessreviews", func(w http.ResponseWriter, r *http.Request) {
		handleSSAR(l.With(zap.String("path", r.URL.Path)), w, r)
	}).Methods(http.MethodPost)
	router.HandleFunc("/apis/{group}/{version}/{resource}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		l := l.With(zap.String("path", r.URL.Path))
		if vars["group"] == "authorization.k8s.io" && vars["resource"] == "selfsubjectaccessreviews" {
			http.Error(w, "this endpoint only supports POST", http.StatusMethodNotAllowed)
			return
		}
		var transformFunc transform.TransformFunc
		if acceptsTable(r) {
			transformFunc = tableTransform(transformKey(vars, transform.VerbList), tableVersion(r))
		}
		if groupResourceMap[groupVersionResource{groupVersion: vars["group"] + "/" + vars["version"], resource: vars["resource"]}].Namespaced {
			if err := response.NewCrossNamespaceListResponse(r, w, filepath.Join(o.baseDir, "namespaces"), vars["group"], vars["resource"], transformFunc); err != nil {
				l.Error("failed to respond", zap.Error(err))
			}
		} else {
			path := path.Join(o.baseDir, "cluster-scoped-resources", vars["group"])
			if err := response.NewListResponse(r, w, path, vars["resource"], transformFunc, nil, filter.FromRequest(r)...); err != nil {
				l.Error("failed to respond", zap.Error(err))
			}
		}
	}).Methods(http.MethodGet)
	router.HandleFunc("/apis/{group}/{version}/{resource}/{name}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		l := l.With(zap.String("path", r.URL.Path))
		path := path.Join(o.baseDir, "cluster-scoped-resources", vars["group"])
		var transformFunc transform.TransformFunc
		if acceptsTable(r) {
			transformFunc = tableTransform(transformKey(vars, transform.VerbList), tableVersion(r))
		}
		if err := response.NewGetResponse(r, w, path, vars["resource"], vars["name"], nil, transformFunc); err != nil {
			l.Error("failed to respond", zap.Error(err))
		}
	}).Methods(http.MethodGet)

	// Re-Define the error handlers so they go through the middleware
	router.NotFoundHandler = router.NewRoute().HandlerFunc(http.NotFound).GetHandler()
	router.MethodNotAllowedHandler = router.NewRoute().HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "", http.StatusMethodNotAllowed) }).GetHandler()

	if err := http.ListenAndServe(":8080", router); err != nil {
		l.Error("server ended", zap.Error(err))
	}
}

func serializeAndWrite(l *zap.Logger, w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	serialized, err := json.Marshal(data)
	if err != nil {
		l.Error("failed to serialize object", zap.String("type", fmt.Sprintf("%T", data)), zap.Error(err))
		return
	}
	if _, err := w.Write(serialized); err != nil {
		l.Error("failed to write object", zap.Error(err))
	}
}

func acceptsTable(r *http.Request) bool {
	return len(r.Header["Accept"]) > 0 && strings.Contains(r.Header["Accept"][0], "as=Table")
}

func tableVersion(r *http.Request) string {
	split := strings.Split(r.Header["Accept"][0], ";")
	for _, s := range split {
		if strings.HasPrefix(s, "v=v") {
			return strings.Split(s, "=")[1]
		}
	}

	return ""
}

func handleSSAR(l *zap.Logger, w http.ResponseWriter, r *http.Request) {
	var ssar authorizationv1.SelfSubjectAccessReview
	if err := json.NewDecoder(r.Body).Decode(&ssar); err != nil {
		w.WriteHeader(400)
		w.Write([]byte(fmt.Sprintf("failed to decode request body: %v", err)))
		return
	}
	ssar.Status.Allowed = true
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(ssar); err != nil {
		l.Error("failed to encode response", zap.Error(err))
	}
}

func loggingMiddleware(l *zap.Logger) mux.MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m := httpsnoop.CaptureMetrics(next, w, r)
			l.Info("Processed request",
				zap.String("method", r.Method),
				zap.String("url", r.URL.String()),
				zap.Int("status", m.Code),
				zap.String("duration", m.Duration.String()),
			)
		})
	}
}

func openFirstFound(paths []string) (*os.File, error) {
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		return f, nil
	}

	return nil, os.ErrNotExist
}

func tailFile(file *os.File, numLines int) ([]byte, error) {
	data, err := ioutil.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("failed to read: %w", err)
	}
	split := bytes.Split(data, []byte("\n"))
	if numLines > len(split) {
		return data, nil
	}

	return bytes.Join(split[len(split)-1-numLines:], []byte("\n")), nil
}

func transformKey(vars map[string]string, verb string) transform.TransformEntryKey {
	return transform.TransformEntryKey{
		ResourceName: vars["resource"],
		GroupName:    vars["group"],
		Version:      vars["version"],
		Verb:         verb,
	}
}

func findByName(l *unstructured.UnstructuredList, name string) *unstructured.Unstructured {
	for _, item := range l.Items {
		if item.GetName() == name {
			return item.DeepCopy()
		}
	}

	return nil
}
