package cluster

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

const fbcPackage = `{"schema":"olm.package","name":"rhods-operator","defaultChannel":"stable"}`

func fbcBundleJSON(name string, images ...string) string {
	var related []map[string]string
	for _, img := range images {
		related = append(related, map[string]string{"name": "img_" + img, "image": "quay.io/rhoai/" + img + "@sha256:aa"})
	}
	b, _ := json.Marshal(map[string]interface{}{"schema": "olm.bundle", "name": name, "package": "rhods-operator", "relatedImages": related})
	return string(b)
}

func prettyJSON(t *testing.T, raw string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(raw), "", "    "); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func imageNames(images []fbcRelatedImage) string {
	var names []string
	for _, img := range images {
		names = append(names, img.Name)
	}
	return strings.Join(names, ",")
}

func TestParseFBCContent_Formats(t *testing.T) {
	b34 := fbcBundleJSON("rhods-operator.3.4.0", "old")
	b35 := fbcBundleJSON("rhods-operator.3.5.0", "dash", "ctrl")
	cases := map[string]string{
		"newline-delimited JSON":     fbcPackage + "\n" + b34 + "\n" + b35 + "\n",
		"pretty concatenated JSON":   prettyJSON(t, fbcPackage) + "\n" + prettyJSON(t, b34) + "\n" + prettyJSON(t, b35) + "\n",
		"concatenated without space": fbcPackage + b34 + b35,
		"YAML documents": `---
schema: olm.package
name: rhods-operator
---
schema: olm.bundle
name: rhods-operator.3.4.0
relatedImages:
  - name: img_old
    image: quay.io/rhoai/old@sha256:aa
---
schema: olm.bundle
name: rhods-operator.3.5.0
relatedImages:
  - name: img_dash
    image: quay.io/rhoai/dash@sha256:aa
  - name: img_ctrl
    image: quay.io/rhoai/ctrl@sha256:aa
`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			images, bundle := parseFBCContent([]byte(content), "rhoai-3.5")
			if bundle != "rhods-operator.3.5.0" || imageNames(images) != "img_dash,img_ctrl" {
				t.Fatalf("got bundle %q images %q", bundle, imageNames(images))
			}
		})
	}
}

func TestParseFBCContent_MalformedJSONLineIsSkipped(t *testing.T) {
	content := fbcPackage + "\n{not json\n" + fbcBundleJSON("rhods-operator.3.5.0", "dash") + "\n"
	images, bundle := parseFBCContent([]byte(content), "rhoai-3.5")
	if bundle != "rhods-operator.3.5.0" || imageNames(images) != "img_dash" {
		t.Fatalf("got bundle %q images %q", bundle, imageNames(images))
	}
}

func TestParseFBCContent_FallbackUsesHighestVersionNotLexicalName(t *testing.T) {
	content := fbcBundleJSON("rhods-operator.3.10.0", "new") + "\n" + fbcBundleJSON("rhods-operator.3.9.0", "old")
	images, bundle := parseFBCContent([]byte(content), "not-a-release-tag")
	if bundle != "rhods-operator.3.10.0" || imageNames(images) != "img_new" {
		t.Fatalf("got bundle %q images %q", bundle, imageNames(images))
	}
}

func TestParseFBCContent_SkipsAnnotationImages(t *testing.T) {
	content := `{"schema":"olm.bundle","name":"rhods-operator.3.5.0","relatedImages":[{"name":"x-annotation","image":"a"},{"name":"keep","image":"b"}]}`
	images, _ := parseFBCContent([]byte(content), "rhoai-3.5")
	if imageNames(images) != "keep" {
		t.Fatalf("got %q", imageNames(images))
	}
}

func gzipTar(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func blobClient(blob []byte) *http.Client {
	return &http.Client{Transport: dscSampleTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(blob))}, nil
	})}
}

func TestDownloadAndParseLayer_ParsesCatalog(t *testing.T) {
	blob := gzipTar(t, map[string][]byte{"configs/rhods-operator/catalog.json": []byte(fbcPackage + "\n" + fbcBundleJSON("rhods-operator.3.5.0", "dash"))})
	images, bundle, err := downloadAndParseLayer(context.Background(), blobClient(blob), "t", "sha256:x", "rhoai-3.5")
	if err != nil || bundle != "rhods-operator.3.5.0" || imageNames(images) != "img_dash" {
		t.Fatalf("got %q %q %v", bundle, imageNames(images), err)
	}
}

func TestDownloadAndParseLayer_TruncatedArchiveIsAnError(t *testing.T) {
	blob := gzipTar(t, map[string][]byte{"configs/rhods-operator/catalog.json": bytes.Repeat([]byte("x"), 4096)})
	_, _, err := downloadAndParseLayer(context.Background(), blobClient(blob[:len(blob)/2]), "t", "sha256:x", "rhoai-3.5")
	if err == nil {
		t.Fatal("a truncated layer must not parse as an empty catalog")
	}
}

func TestSizeLimitedReader(t *testing.T) {
	exact, err := io.ReadAll(&sizeLimitedReader{r: strings.NewReader("12345"), remaining: 5, what: "x"})
	if err != nil || string(exact) != "12345" {
		t.Fatalf("exact size: %q %v", exact, err)
	}
	_, err = io.ReadAll(&sizeLimitedReader{r: strings.NewReader("123456"), remaining: 5, what: "x"})
	if err == nil || !strings.Contains(err.Error(), "exceeds the size limit") {
		t.Fatalf("oversize: want limit error, got %v", err)
	}
}

func TestExtractFBCContent_CachesOnlyDigestPinnedReferences(t *testing.T) {
	resetRegistryCaches()
	t.Cleanup(resetRegistryCaches)
	blob := gzipTar(t, map[string][]byte{"configs/rhods-operator/catalog.json": []byte(fbcBundleJSON("rhods-operator.3.5.0", "dash"))})
	var manifests int32
	original := quayHTTPClient
	quayHTTPClient = &http.Client{Transport: dscSampleTransport(func(r *http.Request) (*http.Response, error) {
		body := []byte(`{"token":"t"}`)
		switch {
		case strings.Contains(r.URL.Path, "/manifests/"):
			atomic.AddInt32(&manifests, 1)
			body = []byte(`{"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[{"digest":"sha256:layer"}]}`)
		case strings.Contains(r.URL.Path, "/blobs/"):
			body = blob
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})}
	defer func() { quayHTTPClient = original }()
	client, cleanup := newMockClient(map[string]mockResponse{})
	defer cleanup()

	tagOnly := "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.5"
	pinned := tagOnly + "@sha256:" + strings.Repeat("a", 64)
	for i := 0; i < 2; i++ {
		for _, ref := range []string{tagOnly, pinned} {
			result, err := ExtractFBCContent(context.Background(), client, ref)
			if err != nil || result.BundleName != "rhods-operator.3.5.0" {
				t.Fatalf("%s: %+v %v", ref, result, err)
			}
		}
	}
	// Tag-only: fetched twice. Digest-pinned: fetched once, then cached.
	if got := atomic.LoadInt32(&manifests); got != 3 {
		t.Fatalf("manifest fetches = %d, want 3", got)
	}
}
