package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	nativeTestBookmarkID     = "bookmark-1"
	nativeTestFavouriteShelf = "Native Favourites"
)

// writeTestEPUB writes a minimal valid EPUB and returns its bytes so tests can
// verify that failed operations leave the original file unchanged.
func writeTestEPUB(t *testing.T, path string) []byte {
	t.Helper()
	var data bytes.Buffer
	w := zip.NewWriter(&data)
	mimetype, err := w.CreateHeader(&zip.FileHeader{Name: "mimetype", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mimetype.Write([]byte("application/epub+zip")); err != nil {
		t.Fatal(err)
	}
	container, err := w.Create("META-INF/container.xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := container.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`)); err != nil {
		t.Fatal(err)
	}
	content, err := w.Create("OEBPS/content.opf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := content.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="bookid">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:identifier id="bookid">test</dc:identifier><dc:title>Test</dc:title><dc:language>en</dc:language></metadata>
  <manifest>
    <item id="chapter" href="chapter.xhtml" media-type="application/xhtml+xml"/>
    <item id="res-cover" href="cover.jpg" media-type="image/jpeg"/>
  </manifest>
  <spine><itemref idref="chapter"/></spine>
</package>`)); err != nil {
		t.Fatal(err)
	}
	chapter, err := w.Create("OEBPS/chapter.xhtml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chapter.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><html xmlns="http://www.w3.org/1999/xhtml"><head><title>Test</title></head><body><p>Hello</p></body></html>`)); err != nil {
		t.Fatal(err)
	}
	cover, err := w.Create("OEBPS/cover.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cover.Write([]byte("test cover")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func readTestZipEntry(t *testing.T, path, entryName string) []byte {
	t.Helper()
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := r.Close(); err != nil {
			t.Errorf("close %s: %v", path, err)
		}
	}()
	data, err := readCoverZipEntry(r, entryName)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestToKepubConvertsBookAndCleansUpTemporaryFiles(t *testing.T) {
	// Given
	outputDir := t.TempDir()
	source := filepath.Join(outputDir, "article.epub")
	destination := filepath.Join(outputDir, "article.kepub.epub")
	writeTestEPUB(t, source)
	// When
	kepubPath, err := toKepub(source, destination)
	// Then
	if err != nil {
		t.Fatalf("toKepub: %v", err)
	}
	if kepubPath != destination {
		t.Fatalf("toKepub returned %q, want %q", kepubPath, destination)
	}
	if err := validateEPUB(kepubPath); err != nil {
		t.Fatalf("converted KEPUB is invalid: %v", err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("source EPUB still exists: %v", err)
	}
	tmpFiles, err := filepath.Glob(filepath.Join(outputDir, ".article.kepub.epub.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tmpFiles) != 0 {
		t.Fatalf("conversion temporary files remain: %v", tmpFiles)
	}
}

func TestToKepubFailurePreservesExistingBook(t *testing.T) {
	// Given
	outputDir := t.TempDir()
	source := filepath.Join(outputDir, "article.epub")
	final := filepath.Join(outputDir, "article.kepub.epub")
	if err := os.WriteFile(source, []byte("invalid source"), 0o600); err != nil {
		t.Fatal(err)
	}
	original := writeTestEPUB(t, final)
	// When
	_, convertErr := toKepub(source, final)
	// Then
	if convertErr == nil {
		t.Fatal("invalid source unexpectedly converted")
	}
	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatal("existing KEPUB was changed after failed conversion")
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source EPUB was not preserved: %v", err)
	}
	tmpFiles, err := filepath.Glob(filepath.Join(outputDir, ".article.kepub.epub.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tmpFiles) != 0 {
		t.Fatalf("conversion temporary files remain: %v", tmpFiles)
	}
}

func TestFixCoverAddsEPUB2AndEPUB3Metadata(t *testing.T) {
	// Given
	path := filepath.Join(t.TempDir(), "article.epub")
	writeTestEPUB(t, path)
	// When
	if err := fixCover(path); err != nil {
		t.Fatalf("fixCover: %v", err)
	}
	// Then
	if err := validateEPUB(path); err != nil {
		t.Fatalf("cover-fixed EPUB is invalid: %v", err)
	}
	opf := string(readTestZipEntry(t, path, "OEBPS/content.opf"))
	if !strings.Contains(opf, `id="res-cover" properties="cover-image"`) {
		t.Fatal("cover manifest item is missing EPUB 3 cover-image property")
	}
	if !strings.Contains(opf, `<meta name="cover" content="res-cover"/>`) {
		t.Fatal("EPUB metadata is missing EPUB 2 cover declaration")
	}
}

func TestFixCoverRenameFailureRemovesTemporaryFile(t *testing.T) {
	// Given
	path := filepath.Join(t.TempDir(), "article.epub")
	original := writeTestEPUB(t, path)
	renameErr := errors.New("rename failed")
	// When
	err := fixCoverWithRename(path, func(oldPath, newPath string) error {
		if oldPath != path+".covertmp" || newPath != path {
			t.Errorf("rename paths = %q, %q", oldPath, newPath)
		}
		return renameErr
	})
	// Then
	if !errors.Is(err, renameErr) {
		t.Fatalf("fixCoverWithRename error = %v, want rename error", err)
	}
	if _, err := os.Stat(path + ".covertmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cover temporary file remains: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatal("original EPUB changed after failed cover rename")
	}
}
