package podman

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// bigData writes containers/storage's big data item key for image id, as
// the file it keeps it in: "=" + base64(key).
func bigData(t *testing.T, imagesDir, id, key, body string) {
	t.Helper()
	dir := filepath.Join(imagesDir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "="+base64.StdEncoding.EncodeToString([]byte(key))), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const (
	imgID    = "ab3fe4defd29ba6231229a4d41440ac8bde8218e85870e53876277faa24b35c4"
	listDig  = "sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc"
	armDig   = "sha256:45e09956dc667c5eff3583c9d94830261fb1ca0be10a0a7db36266edf5de9e1d"
	ociIndex = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[` +
		`{"digest":"sha256:1","platform":{"architecture":"amd64","os":"linux"}},` +
		`{"digest":"sha256:2","platform":{"architecture":"unknown","os":"unknown"}},` +
		`{"digest":"sha256:3","platform":{"architecture":"arm","os":"linux","variant":"v7"}},` +
		`{"digest":"` + armDig + `","platform":{"architecture":"arm64","os":"linux","variant":"v8"}}]}`
	armManifest = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:` + imgID + `"},"layers":[]}`
	armConfig   = `{"architecture":"arm64","os":"linux","variant":"v8","config":{}}`
)

// A pulled multi-arch image: the index Podman stored beside it lists every
// platform the tag offers, attestation entries (unknown/unknown) left out.
// The same image pulled by its platform manifest's digest offers only that.
func TestImagePlatformsFromStoredIndex(t *testing.T) {
	dir := t.TempDir()
	bigData(t, dir, imgID, "manifest-"+listDig, ociIndex)
	bigData(t, dir, imgID, "manifest-"+armDig, armManifest)
	bigData(t, dir, imgID, "manifest", armManifest)
	bigData(t, dir, imgID, "sha256:"+imgID, armConfig)
	info := ImageInfo{ID: imgID, OS: "linux", Architecture: "arm64"}

	for ref, want := range map[string]string{
		"docker.io/library/alpine:3.20":           "[linux/amd64 linux/arm/v7 linux/arm64/v8]",
		"docker.io/library/alpine@" + listDig:     "[linux/amd64 linux/arm/v7 linux/arm64/v8]",
		"docker.io/library/alpine@" + armDig:      "[linux/arm64/v8]",
		"docker.io/library/alpine:3.20@" + armDig: "[linux/arm64/v8]",
	} {
		got, err := ImagePlatforms(dir, ref, info)
		if err != nil || fmt.Sprint(got) != want {
			t.Errorf("%s: %v %v, want %s", ref, got, err, want)
		}
	}
}

// A single-arch image (no list stored) offers its own platform, variant
// included from its config; without a config, the inspect's platform. An
// image Podman has no directory for is an error, never a guess.
func TestImagePlatformsSingleArch(t *testing.T) {
	dir := t.TempDir()
	bigData(t, dir, imgID, "manifest-"+armDig, armManifest)
	bigData(t, dir, imgID, "sha256:"+imgID, armConfig)
	if got, err := ImagePlatforms(dir, "quay.io/x/y:1", ImageInfo{ID: "sha256:" + imgID}); err != nil || fmt.Sprint(got) != "[linux/arm64/v8]" {
		t.Fatalf("single-arch: %v %v", got, err)
	}

	other := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	bigData(t, dir, other, "manifest", armManifest)
	if got, err := ImagePlatforms(dir, "quay.io/x/z:1", ImageInfo{ID: other, OS: "linux", Architecture: "amd64"}); err != nil || fmt.Sprint(got) != "[linux/amd64]" {
		t.Fatalf("from inspect: %v %v", got, err)
	}

	if got, err := ImagePlatforms(dir, "quay.io/x/gone:1", ImageInfo{ID: "feed", OS: "linux", Architecture: "amd64"}); err == nil {
		t.Fatalf("no image directory: %v, want an error", got)
	}
}
