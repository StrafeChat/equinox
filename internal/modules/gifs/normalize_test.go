package gifs

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustNormalize(t *testing.T, body string, offset int) Page {
	t.Helper()
	page, err := normalize([]byte(body), offset)
	if err != nil {
		t.Fatalf("normalize(%s): %v", body, err)
	}
	return page
}

// Rendition preference matches what the web client used: the grid gets fixed_width first,
// the message gets downsized first, the placeholder gets fixed_width_still first - and each
// falls through its list in order when a rendition is missing or has no URL.
func TestNormalizeRenditionPreference(t *testing.T) {
	body := `{"data":[
		{"id":"all","title":"All renditions","images":{
			"original":{"url":"https://m.example/original.gif","width":"480","height":"270"},
			"downsized":{"url":"https://m.example/downsized.gif","width":"400","height":"225"},
			"downsized_medium":{"url":"https://m.example/medium.gif"},
			"fixed_width":{"url":"https://m.example/fw.gif","width":"200","height":"113"},
			"fixed_height":{"url":"https://m.example/fh.gif"},
			"preview_gif":{"url":"https://m.example/preview.gif"},
			"fixed_width_still":{"url":"https://m.example/fw_s.gif"},
			"original_still":{"url":"https://m.example/o_s.gif"}}},
		{"id":"height","images":{
			"fixed_height":{"url":"https://m.example/fh.gif"},
			"original":{"url":"https://m.example/original.gif"},
			"original_still":{"url":"https://m.example/o_s.gif"}}},
		{"id":"preview-only","images":{"preview_gif":{"url":"https://m.example/preview.gif","width":100,"height":50}}},
		{"id":"empty-first","images":{
			"fixed_width":{"url":""},
			"fixed_width_downsampled":{"url":"https://m.example/fwd.gif"},
			"downsized":{"url":12},
			"downsized_medium":{"url":"https://m.example/medium.gif"}}}
	]}`
	page := mustNormalize(t, body, 0)
	if len(page.Results) != 4 {
		t.Fatalf("want 4 results, got %d: %+v", len(page.Results), page.Results)
	}
	type want struct{ preview, full, still string }
	cases := []want{
		{"https://m.example/fw.gif", "https://m.example/downsized.gif", "https://m.example/fw_s.gif"},
		{"https://m.example/fh.gif", "https://m.example/original.gif", "https://m.example/o_s.gif"},
		// No full rendition at all: the preview is sent as the message.
		{"https://m.example/preview.gif", "https://m.example/preview.gif", ""},
		// An empty URL and a URL that is not a string both count as "missing".
		{"https://m.example/fwd.gif", "https://m.example/medium.gif", ""},
	}
	for i, w := range cases {
		r := page.Results[i]
		if r.PreviewURL != w.preview || r.URL != w.full || r.StillURL != w.still {
			t.Errorf("result %d (%s): preview=%q full=%q still=%q, want %+v", i, r.ID, r.PreviewURL, r.URL, r.StillURL, w)
		}
	}
	r := page.Results[0]
	if r.Title != "All renditions" || r.Width != 400 || r.Height != 225 || r.PreviewWidth != 200 || r.PreviewHeight != 113 {
		t.Errorf("sizes/title of the first result: %+v", r)
	}
}

// Sizes come as strings from GIPHY and as numbers from some compatible APIs; anything
// unreadable is 0 rather than a failed page.
func TestNormalizeSizes(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{`"200"`, 200},
		{`113`, 113},
		{`12.7`, 12},
		{`" 64 "`, 64},
		{`"200px"`, 200},
		{`"abc"`, 0},
		{`""`, 0},
		{`null`, 0},
		{`-5`, 0},
		{`"-5"`, 0},
		{`true`, 0},
		{`{"x":1}`, 0},
		{`"99999999999999999999999"`, 0},
		{`1e300`, 1 << 20},
	}
	for _, c := range cases {
		body := `{"data":[{"id":"x","images":{"fixed_width":{"url":"https://m.example/a.gif","width":` + c.raw + `,"height":` + c.raw + `}}}]}`
		page := mustNormalize(t, body, 0)
		if len(page.Results) != 1 {
			t.Fatalf("size %s: item dropped", c.raw)
		}
		r := page.Results[0]
		if r.Width != c.want || r.Height != c.want || r.PreviewWidth != c.want || r.PreviewHeight != c.want {
			t.Errorf("size %s: got %d/%d, want %d", c.raw, r.Width, r.Height, c.want)
		}
	}
}

// Only https URLs reach the client. A preview or full rendition that is anything else drops
// the item; a still that is not https is just left out.
func TestNormalizeHTTPSOnly(t *testing.T) {
	body := `{"data":[
		{"id":"ok","images":{"fixed_width":{"url":"https://m.example/a.gif"},"fixed_width_still":{"url":"http://m.example/a_s.gif"}}},
		{"id":"http-preview","images":{"fixed_width":{"url":"http://m.example/a.gif"},"downsized":{"url":"https://m.example/b.gif"}}},
		{"id":"http-full","images":{"fixed_width":{"url":"https://m.example/a.gif"},"downsized":{"url":"http://m.example/b.gif"}}},
		{"id":"js","images":{"fixed_width":{"url":"javascript:alert(1)"}}},
		{"id":"data","images":{"fixed_width":{"url":"data:image/gif;base64,R0lGOD"}}},
		{"id":"no-host","images":{"fixed_width":{"url":"https:///a.gif"}}},
		{"id":"relative","images":{"fixed_width":{"url":"/a.gif"}}},
		{"id":"upper","images":{"fixed_width":{"url":"HTTPS://m.example/c.gif"}}}
	]}`
	page := mustNormalize(t, body, 0)
	var ids []string
	for _, r := range page.Results {
		ids = append(ids, r.ID)
	}
	if strings.Join(ids, ",") != "ok,upper" {
		t.Fatalf("kept %v, want [ok upper]", ids)
	}
	if page.Results[0].StillURL != "" {
		t.Errorf("an http still must be left out, got %q", page.Results[0].StillURL)
	}
}

// Items the client could not show are dropped one by one; the rest of the page survives.
func TestNormalizeDropsUnusableItems(t *testing.T) {
	body := `{"data":[
		null,
		42,
		"a string",
		{"id":"no-images"},
		{"id":"empty-images","images":{}},
		{"id":"images-not-object","images":[1,2]},
		{"id":"all-urls-empty","images":{"fixed_width":{"url":""},"original":{}}},
		{"id":"only-still","images":{"fixed_width_still":{"url":"https://m.example/s.gif"}}},
		{"id":"keep","images":{"original":{"url":"https://m.example/o.gif"}}}
	]}`
	page := mustNormalize(t, body, 0)
	if len(page.Results) != 1 || page.Results[0].ID != "keep" {
		t.Fatalf("want only the usable item, got %+v", page.Results)
	}
}

func TestNormalizeIDAndTitle(t *testing.T) {
	body := `{"data":[
		{"id":"abc","title":"Cat","images":{"fixed_width":{"url":"https://m.example/1.gif"}}},
		{"id":12345,"title":7,"images":{"fixed_width":{"url":"https://m.example/2.gif"}}},
		{"images":{"fixed_width":{"url":"https://m.example/3.gif"}}},
		{"id":null,"images":{"fixed_width":{"url":"https://m.example/4.gif"}}},
		{"id":"","images":{"fixed_width":{"url":"https://m.example/5.gif"}}}
	]}`
	page := mustNormalize(t, body, 0)
	want := []struct{ id, title string }{
		{"abc", "Cat"},
		{"12345", ""},
		{"https://m.example/3.gif", ""},
		{"https://m.example/4.gif", ""},
		{"https://m.example/5.gif", ""},
	}
	if len(page.Results) != len(want) {
		t.Fatalf("got %d results", len(page.Results))
	}
	for i, w := range want {
		if r := page.Results[i]; r.ID != w.id || r.Title != w.title {
			t.Errorf("result %d: id=%q title=%q, want %+v", i, r.ID, r.Title, w)
		}
	}
}

func TestNormalizePagination(t *testing.T) {
	item := `{"id":"x","images":{"fixed_width":{"url":"https://m.example/a.gif"}}}`
	two := `[` + item + `,` + item + `]`
	cases := []struct {
		name        string
		body        string
		offset      int
		next, total int
	}{
		{"provider pagination", `{"data":` + two + `,"pagination":{"offset":10,"count":2,"total_count":100}}`, 10, 12, 100},
		// count is what the provider returned, including items dropped here: the next page
		// starts after all of them.
		{"count beats kept results", `{"data":` + two + `,"pagination":{"offset":0,"count":5,"total_count":100}}`, 0, 5, 100},
		{"no pagination", `{"data":` + two + `}`, 24, 26, 28},
		{"pagination not numbers", `{"data":` + two + `,"pagination":{"offset":"10","count":"2","total_count":"100"}}`, 24, 26, 28},
		{"pagination not an object", `{"data":` + two + `,"pagination":"nope"}`, 0, 2, 4},
		{"total capped to what can be paged", `{"data":` + two + `,"pagination":{"offset":0,"count":2,"total_count":250000}}`, 0, 2, maxOffset + 1},
		{"no data", `{"data":null}`, 7, 7, 7},
	}
	for _, c := range cases {
		page := mustNormalize(t, c.body, c.offset)
		if page.NextOffset != c.next || page.TotalCount != c.total {
			t.Errorf("%s: next=%d total=%d, want %d/%d", c.name, page.NextOffset, page.TotalCount, c.next, c.total)
		}
		if page.Results == nil {
			t.Errorf("%s: results must be an empty list, not null", c.name)
		}
	}
}

func TestNormalizeRejectsWrongShape(t *testing.T) {
	for _, body := range []string{`not json`, `{"data":{}}`, `[]`, ``} {
		if _, err := normalize([]byte(body), 0); err == nil {
			t.Errorf("normalize(%q) accepted a body that is not a GIPHY answer", body)
		}
	}
}

// The response shape is the contract the web and mobile clients build against.
func TestPageJSONShape(t *testing.T) {
	page := Page{Results: []Result{{ID: "a", Title: "t", URL: "https://u", Width: 1, Height: 2,
		PreviewURL: "https://p", PreviewWidth: 3, PreviewHeight: 4}}, NextOffset: 5, TotalCount: 6}
	b, _ := json.Marshal(page)
	want := `{"results":[{"id":"a","title":"t","url":"https://u","width":1,"height":2,"preview_url":"https://p","preview_width":3,"preview_height":4}],"next_offset":5,"total_count":6}`
	if string(b) != want {
		t.Errorf("JSON =\n%s\nwant\n%s", b, want)
	}
	page.Results[0].StillURL = "https://s"
	if b, _ := json.Marshal(page); !strings.Contains(string(b), `"still_url":"https://s"`) {
		t.Errorf("still_url missing: %s", b)
	}
}
