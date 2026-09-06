package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeBookmarkStore struct {
	bookmark readeckBookmark
	patches  []map[string]any
	gets     int
	patchErr error
}

func (store *fakeBookmarkStore) getBookmark(id string) (readeckBookmark, error) {
	store.gets++
	return store.bookmark, nil
}

func (store *fakeBookmarkStore) patchBookmark(id string, fields map[string]any) error {
	store.patches = append(store.patches, fields)
	if store.patchErr != nil {
		return store.patchErr
	}
	if progress, ok := fields["read_progress"].(int); ok {
		store.bookmark.ReadProgress = progress
	}
	if archived, ok := fields["is_archived"].(bool); ok {
		store.bookmark.IsArchived = archived
	}
	if marked, ok := fields["is_marked"].(bool); ok {
		store.bookmark.IsMarked = marked
	}
	return nil
}

type fakeNickelLibrary struct {
	status        bookStatus
	inCollection  bool
	statusErr     error
	collectionErr error
}

func (library fakeNickelLibrary) readStatus(id, outputDir string) (bookStatus, error) {
	return library.status, library.statusErr
}

func (library fakeNickelLibrary) isInCollection(id, outputDir, collection string) (bool, error) {
	return library.inCollection, library.collectionErr
}

type localBookReconcileTest struct {
	t              *testing.T
	readeck        fakeBookmarkStore
	nickel         fakeNickelLibrary
	cfg            appConfig
	book           localBook
	valid          bool
	listedBookmark bool
}

func newLocalBookReconcileTest(t *testing.T) *localBookReconcileTest {
	t.Helper()
	outputDir := t.TempDir()
	bookPath := filepath.Join(outputDir, nativeTestBookmarkID+".kepub.epub")
	if err := os.WriteFile(bookPath, []byte("native fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &localBookReconcileTest{
		t:       t,
		readeck: fakeBookmarkStore{bookmark: readeckBookmark{ID: nativeTestBookmarkID}},
		nickel:  fakeNickelLibrary{status: bookUnread},
		cfg:     appConfig{Output: outputConfig{Path: outputDir}},
		book:    localBook{id: nativeTestBookmarkID, path: bookPath},
	}
}

func (h *localBookReconcileTest) bookWasFetched()   { h.valid = true; h.listedBookmark = true }
func (h *localBookReconcileTest) archiveReadBooks() { h.cfg.Sync.Archive = true }
func (h *localBookReconcileTest) syncFavourites() {
	h.cfg.Sync.FavouriteCollection = nativeTestFavouriteShelf
}
func (h *localBookReconcileTest) bookIsInKoboCollection() { h.nickel.inCollection = true }
func (h *localBookReconcileTest) deleteStaleFiles()       { h.cfg.Output.Delete = true }

func (h *localBookReconcileTest) remoteBookmark(bookmark readeckBookmark) {
	if bookmark.ID == "" {
		bookmark.ID = nativeTestBookmarkID
	}
	h.readeck.bookmark = bookmark
}

type reconcileResult struct {
	state        readeckBookmark
	filesChanged bool
	patches      int
	gets         int
	removed      bool
}

func (h *localBookReconcileTest) run(status bookStatus) reconcileResult {
	h.t.Helper()
	h.nickel.status = status
	valid := make(map[string]bool)
	if h.valid {
		valid[nativeTestBookmarkID] = true
	}
	bookmarks := make(map[string]readeckBookmark)
	if h.listedBookmark {
		bookmarks[nativeTestBookmarkID] = h.readeck.bookmark
	}

	filesChanged, err := reconcileLocalBook(&h.readeck, h.nickel, h.cfg, h.cfg.Output.Path, h.book, valid, bookmarks, h.cfg.Output.Delete)
	if err != nil {
		h.t.Fatal(err)
	}
	_, statErr := os.Stat(h.book.path)
	return reconcileResult{state: h.readeck.bookmark, filesChanged: filesChanged, patches: len(h.readeck.patches), gets: h.readeck.gets, removed: os.IsNotExist(statErr)}
}

func requireReadeckState(t *testing.T, got readeckBookmark, readProgress int, archived, marked bool) {
	t.Helper()
	if got.ReadProgress != readProgress || got.IsArchived != archived || got.IsMarked != marked {
		t.Fatalf("unexpected Readeck state: %+v", got)
	}
}

func requireAPICalls(t *testing.T, got reconcileResult, patches, gets int) {
	t.Helper()
	if got.patches != patches || got.gets != gets {
		t.Fatalf("API calls: got %d PATCH and %d GET, want %d PATCH and %d GET", got.patches, got.gets, patches, gets)
	}
}

func requireLocalFile(t *testing.T, got reconcileResult, wantFile bool) {
	t.Helper()
	if wantFile && got.removed {
		t.Fatal("local book was removed")
	}
	if !wantFile && !got.removed {
		t.Fatal("local book was not removed")
	}
}

func TestReconcileMarksFinishedBookmarkReadArchivedAndFavourited(t *testing.T) {
	// Given
	reconcile := newLocalBookReconcileTest(t)
	reconcile.bookWasFetched()
	reconcile.archiveReadBooks()
	reconcile.syncFavourites()
	reconcile.bookIsInKoboCollection()
	// When
	got := reconcile.run(bookRead)
	// Then
	requireReadeckState(t, got.state, 100, true, true)
	requireAPICalls(t, got, 2, 0)
	requireLocalFile(t, got, true)
}

func TestReconcileMarksFinishedBookmarkReadWithoutArchiving(t *testing.T) {
	// Given
	reconcile := newLocalBookReconcileTest(t)
	reconcile.bookWasFetched()
	reconcile.syncFavourites()
	// When
	got := reconcile.run(bookRead)
	// Then
	requireReadeckState(t, got.state, 100, false, false)
	requireAPICalls(t, got, 1, 0)
	requireLocalFile(t, got, true)
}

func TestReconcileMarksFinishedBookOutsideFetchedFeedRead(t *testing.T) {
	// Given
	reconcile := newLocalBookReconcileTest(t)
	reconcile.archiveReadBooks()
	// When
	got := reconcile.run(bookRead)
	// Then
	requireReadeckState(t, got.state, 100, true, false)
	requireAPICalls(t, got, 1, 1)
	requireLocalFile(t, got, true)
}

func TestReconcileUnfavoursArchivedBookmark(t *testing.T) {
	// Given
	reconcile := newLocalBookReconcileTest(t)
	reconcile.syncFavourites()
	reconcile.remoteBookmark(readeckBookmark{IsArchived: true, IsMarked: true})
	// When
	got := reconcile.run(bookUnread)
	// Then
	requireReadeckState(t, got.state, 0, true, false)
	requireAPICalls(t, got, 1, 1)
	requireLocalFile(t, got, true)
}

func TestReconcileDeletesStaleUnreadBookmark(t *testing.T) {
	// Given
	reconcile := newLocalBookReconcileTest(t)
	reconcile.deleteStaleFiles()
	// When
	got := reconcile.run(bookUnread)
	// Then
	requireLocalFile(t, got, false)
	if !got.filesChanged {
		t.Fatal("deleted stale bookmark was not reported as a filesystem change")
	}
}

func TestReconcileDoesNotDeleteWhenDeletionIsDisallowed(t *testing.T) {
	// Given
	reconcile := newLocalBookReconcileTest(t)
	reconcile.deleteStaleFiles()
	valid := make(map[string]bool)
	bookmarks := make(map[string]readeckBookmark)
	// When
	filesChanged, err := reconcileLocalFiles(&reconcile.readeck, reconcile.nickel, reconcile.cfg, valid, bookmarks, false)
	// Then
	if err != nil {
		t.Fatalf("reconcileLocalFiles: %v", err)
	}
	if _, err := os.Stat(reconcile.book.path); err != nil {
		t.Fatalf("stale local book was deleted: %v", err)
	}
	if filesChanged {
		t.Fatal("retained stale bookmark was reported as a filesystem change")
	}
}

func TestReconcilePropagatesStatusError(t *testing.T) {
	// Given
	reconcile := newLocalBookReconcileTest(t)
	reconcile.nickel.statusErr = errors.New("status unavailable")
	// When
	_, err := reconcileLocalBook(&reconcile.readeck, reconcile.nickel, reconcile.cfg, reconcile.cfg.Output.Path, reconcile.book, make(map[string]bool), make(map[string]readeckBookmark), false)
	// Then
	if err == nil || !strings.Contains(err.Error(), "status unavailable") {
		t.Fatalf("status error = %v, want propagated status error", err)
	}
}

func TestReconcileReportsMarkReadFailure(t *testing.T) {
	// Given
	reconcile := newLocalBookReconcileTest(t)
	reconcile.bookWasFetched()
	reconcile.nickel.status = bookRead
	reconcile.readeck.patchErr = errors.New("remote update failed")
	// When
	_, err := reconcileLocalBook(&reconcile.readeck, reconcile.nickel, reconcile.cfg, reconcile.cfg.Output.Path, reconcile.book, map[string]bool{nativeTestBookmarkID: true}, map[string]readeckBookmark{nativeTestBookmarkID: reconcile.readeck.bookmark}, false)
	// Then
	if err == nil || !strings.Contains(err.Error(), "remote update failed") {
		t.Fatalf("mark-read error = %v, want remote update error", err)
	}
}

func TestReconcilePreservesLocalBookWhenMarkReadFails(t *testing.T) {
	// Given
	reconcile := newLocalBookReconcileTest(t)
	reconcile.deleteStaleFiles()
	reconcile.nickel.status = bookRead
	remoteErr := errors.New("remote update failed")
	reconcile.readeck.patchErr = remoteErr
	// When
	filesChanged, err := reconcileLocalBook(
		&reconcile.readeck,
		reconcile.nickel,
		reconcile.cfg,
		reconcile.cfg.Output.Path,
		reconcile.book,
		make(map[string]bool),
		make(map[string]readeckBookmark),
		true,
	)
	// Then
	if !errors.Is(err, remoteErr) {
		t.Fatalf("reconciliation error = %v, want remote update error", err)
	}
	if filesChanged {
		t.Fatal("retained book was reported as a filesystem change")
	}
	if _, err := os.Stat(reconcile.book.path); err != nil {
		t.Fatalf("local retry input was not preserved: %v", err)
	}
}

func TestReconcilePreservesBookMissingFromFetchWhileReading(t *testing.T) {
	// Given
	reconcile := newLocalBookReconcileTest(t)
	reconcile.deleteStaleFiles()
	// When
	got := reconcile.run(bookReading)
	// Then
	requireLocalFile(t, got, true)
	if got.filesChanged {
		t.Fatal("retained in-progress bookmark was reported as a filesystem change")
	}
}

func TestReconcilePreservesBookMissingFromFetchWhenClosed(t *testing.T) {
	// Given
	reconcile := newLocalBookReconcileTest(t)
	reconcile.deleteStaleFiles()
	// When
	got := reconcile.run(bookClosed)
	// Then
	requireLocalFile(t, got, true)
	if got.filesChanged {
		t.Fatal("retained closed bookmark was reported as a filesystem change")
	}
}
