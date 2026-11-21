package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	bundlelib "github.com/jvzantvoort/bundle/bundle"
	"github.com/jvzantvoort/bundle/tag"
)

// addBundle creates a bundle with the given tags in the default pool.
func addBundle(t *testing.T, app *App, title string, tags ...string) string {
	t.Helper()
	source := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, title+".txt"), []byte(title), 0o644); err != nil {
		t.Fatal(err)
	}
	created, err := bundlelib.Create(source, title)
	if err != nil {
		t.Fatal(err)
	}
	set := &tag.Tags{}
	set.Add(tags...)
	if err := set.Save(source); err != nil {
		t.Fatal(err)
	}
	checksum := created.Metadata.BundleChecksum
	if err := os.Rename(source, filepath.Join(app.config.Pools["default"].Root, checksum)); err != nil {
		t.Fatal(err)
	}
	app.invalidateMetadata("default")
	return checksum
}

func call(t *testing.T, handler http.Handler, method, target, body, ifMatch string, out any) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if out != nil && response.Code < 300 {
		if err := json.Unmarshal(response.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, target, err, response.Body.String())
		}
	}
	return response
}

func etag(t *testing.T, handler http.Handler, checksum string) string {
	t.Helper()
	response := call(t, handler, http.MethodGet, "/api/v1/pools/default/bundles/"+checksum, "", "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("get bundle = %d", response.Code)
	}
	return response.Header().Get("ETag")
}

func bundleTagsOf(t *testing.T, handler http.Handler, checksum string) []string {
	t.Helper()
	var tags tagsResponse
	call(t, handler, http.MethodGet, "/api/v1/pools/default/bundles/"+checksum+"/tags", "", "", &tags)
	return tags.Tags
}

func TestListAndQueryIncludeTags(t *testing.T) {
	app, _ := testApp(t)
	handler := app.Handler()
	checksum := addBundle(t, app, "tagged", "travel", "2024")

	var page struct {
		Items []bundleSummary `json:"items"`
	}
	call(t, handler, http.MethodGet, "/api/v1/pools/default/bundles?title=tagged", "", "", &page)
	if len(page.Items) != 1 || !reflect.DeepEqual(page.Items[0].Tags, []string{"2024", "travel"}) {
		t.Fatalf("list items = %+v, want tags [2024 travel]", page.Items)
	}

	var query struct {
		Items []struct {
			Bundle bundleSummary `json:"bundle"`
		} `json:"items"`
	}
	call(t, handler, http.MethodPost, "/api/v1/pools/default/bundles/query", `{"checksums":["`+checksum+`"]}`, "", &query)
	if len(query.Items) != 1 || !reflect.DeepEqual(query.Items[0].Bundle.Tags, []string{"2024", "travel"}) {
		t.Fatalf("query items = %+v, want tags [2024 travel]", query.Items)
	}

	// A bundle without tags has an empty list, not null
	response := call(t, handler, http.MethodGet, "/api/v1/pools/default/bundles?title=test", "", "", nil)
	if !strings.Contains(response.Body.String(), `"tags":[]`) {
		t.Fatalf("untagged bundle in list: %s", response.Body.String())
	}
}

func TestAddingInvalidTagIsRefused(t *testing.T) {
	app, _ := testApp(t)
	handler := app.Handler()
	checksum := addBundle(t, app, "photos", "travel")

	response := call(t, handler, http.MethodPost, "/api/v1/pools/default/bundles/"+checksum+"/tags", `{"tags":["ok","not valid!"]}`, etag(t, handler, checksum), nil)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("add invalid tag = %d, want 400: %s", response.Code, response.Body.String())
	}
	if got := bundleTagsOf(t, handler, checksum); !reflect.DeepEqual(got, []string{"travel"}) {
		t.Fatalf("tags after refused add = %v, want [travel]", got)
	}
}

func TestRenameBundleTag(t *testing.T) {
	app, _ := testApp(t)
	handler := app.Handler()
	checksum := addBundle(t, app, "photos", "travel", "holiday", "trip")
	target := "/api/v1/pools/default/bundles/" + checksum + "/tags/"

	if response := call(t, handler, http.MethodPut, target+"travel", `{"tag":"journey"}`, "", nil); response.Code != http.StatusPreconditionFailed {
		t.Fatalf("rename without If-Match = %d, want 412", response.Code)
	}
	if response := call(t, handler, http.MethodPut, target+"missing", `{"tag":"journey"}`, etag(t, handler, checksum), nil); response.Code != http.StatusNotFound {
		t.Fatalf("rename of absent tag = %d, want 404", response.Code)
	}
	if response := call(t, handler, http.MethodPut, target+"travel", `{"tag":"bad tag"}`, etag(t, handler, checksum), nil); response.Code != http.StatusBadRequest {
		t.Fatalf("rename to invalid tag = %d, want 400", response.Code)
	}

	// Case is ignored, and the response carries the new ETag
	var renamed tagsResponse
	response := call(t, handler, http.MethodPut, target+"Travel", `{"tag":"Journey"}`, etag(t, handler, checksum), &renamed)
	if response.Code != http.StatusOK || !reflect.DeepEqual(renamed.Tags, []string{"holiday", "journey", "trip"}) {
		t.Fatalf("rename = %d %+v, want [holiday journey trip]", response.Code, renamed)
	}
	if response.Header().Get("ETag") != etag(t, handler, checksum) {
		t.Fatal("rename did not return the current ETag")
	}

	// Renaming onto a tag the bundle already has merges the two
	call(t, handler, http.MethodPut, target+"trip", `{"tag":"holiday"}`, response.Header().Get("ETag"), &renamed)
	if !reflect.DeepEqual(renamed.Tags, []string{"holiday", "journey"}) {
		t.Fatalf("merge rename = %v, want [holiday journey]", renamed.Tags)
	}
}

func TestPoolTags(t *testing.T) {
	app, _ := testApp(t)
	handler := app.Handler()
	a := addBundle(t, app, "a", "travel", "2024")
	b := addBundle(t, app, "b", "travel", "trip")
	c := addBundle(t, app, "c", "work")

	var list struct {
		Items []poolTag `json:"items"`
		Total int       `json:"total"`
	}
	call(t, handler, http.MethodGet, "/api/v1/pools/default/tags", "", "", &list)
	want := []poolTag{{"2024", 1}, {"travel", 2}, {"trip", 1}, {"work", 1}}
	if list.Total != 4 || !reflect.DeepEqual(list.Items, want) {
		t.Fatalf("pool tags = %+v, want %+v", list, want)
	}

	// Fill the cache, then change b behind its back: the rename must not trust it
	if _, err := app.refreshMetadata("default"); err != nil {
		t.Fatal(err)
	}
	stale := &tag.Tags{Tags: []string{"other"}}
	if err := stale.Save(filepath.Join(app.config.Pools["default"].Root, b)); err != nil {
		t.Fatal(err)
	}

	var result tagChangeResult
	call(t, handler, http.MethodPut, "/api/v1/pools/default/tags/travel", `{"tag":"trip"}`, "", &result)
	if result.Changed != 1 || len(result.Failed) != 0 || result.Tag != "travel" || result.NewTag != "trip" {
		t.Fatalf("pool rename = %+v, want 1 changed", result)
	}
	if got := bundleTagsOf(t, handler, a); !reflect.DeepEqual(got, []string{"2024", "trip"}) {
		t.Fatalf("tags of a = %v, want [2024 trip]", got)
	}
	if got := bundleTagsOf(t, handler, b); !reflect.DeepEqual(got, []string{"other"}) {
		t.Fatalf("tags of b = %v, want [other]", got)
	}

	result = tagChangeResult{}
	call(t, handler, http.MethodDelete, "/api/v1/pools/default/tags/work", "", "", &result)
	if result.Changed != 1 || result.NewTag != "" {
		t.Fatalf("pool delete = %+v, want 1 changed", result)
	}
	if got := bundleTagsOf(t, handler, c); len(got) != 0 {
		t.Fatalf("tags of c = %v, want none", got)
	}

	// Repeating finds nothing left to change
	result = tagChangeResult{}
	call(t, handler, http.MethodDelete, "/api/v1/pools/default/tags/work", "", "", &result)
	if result.Changed != 0 {
		t.Fatalf("repeated delete = %+v, want 0 changed", result)
	}

	for _, tc := range []struct{ method, target, body string }{
		{http.MethodPut, "/api/v1/pools/default/tags/bad%20tag", `{"tag":"x"}`},
		{http.MethodPut, "/api/v1/pools/default/tags/x", `{"tag":""}`},
		{http.MethodDelete, "/api/v1/pools/default/tags/bad%20tag", ""},
	} {
		if response := call(t, handler, tc.method, tc.target, tc.body, "", nil); response.Code != http.StatusBadRequest {
			t.Errorf("%s %s = %d, want 400", tc.method, tc.target, response.Code)
		}
	}
	if response := call(t, handler, http.MethodPut, "/api/v1/pools/nope/tags/x", `{"tag":"y"}`, "", nil); response.Code != http.StatusNotFound {
		t.Errorf("rename in unknown pool = %d, want 404", response.Code)
	}
}

func TestCapabilitiesReportAPIVersion(t *testing.T) {
	app, _ := testApp(t)
	var capabilities struct {
		APIVersion string `json:"api_version"`
	}
	call(t, app.Handler(), http.MethodGet, "/api/v1/capabilities", "", "", &capabilities)
	if capabilities.APIVersion != apiVersion {
		t.Fatalf("api_version = %q, want %q", capabilities.APIVersion, apiVersion)
	}
}
