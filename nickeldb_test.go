package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestNickelReadStatusReadsKnownStatusesFromNickelSchema(t *testing.T) {
	// Given
	db := openNickelSchemaFixture(t)
	outputDir := t.TempDir()

	tests := []struct {
		name string
		id   string
		want bookStatus
	}{
		{name: "unread", id: "unread", want: bookUnread},
		{name: "reading", id: "reading", want: bookReading},
		{name: "read", id: "read", want: bookRead},
		{name: "closed", id: "closed", want: bookClosed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Given
			insertNickelContent(t, db, outputDir, test.id, test.want)
			// When
			got, err := nickelReadStatus(db, test.id, outputDir, false)
			// Then
			if err != nil {
				t.Fatalf("nickelReadStatus: %v", err)
			}
			if got != test.want {
				t.Fatalf("nickelReadStatus = %d, want %d", got, test.want)
			}
		})
	}
}

func TestNickelReadStatusMissingBookIsUnread(t *testing.T) {
	// Given
	db := openNickelSchemaFixture(t)
	// When
	got, err := nickelReadStatus(db, "missing", t.TempDir(), false)
	// Then
	if err != nil {
		t.Fatalf("nickelReadStatus: %v", err)
	}
	if got != bookUnread {
		t.Fatalf("nickelReadStatus = %d, want %d", got, bookUnread)
	}
}

func TestNickelIsInCollectionUsesSchemaFixture(t *testing.T) {
	// Given
	db := openNickelSchemaFixture(t)
	outputDir := t.TempDir()
	insertNickelContent(t, db, outputDir, nativeTestBookmarkID, bookUnread)
	insertNickelCollection(t, db, outputDir, nativeTestBookmarkID, nativeTestFavouriteShelf)
	// When
	got, err := nickelIsInCollection(db, nativeTestBookmarkID, outputDir, nativeTestFavouriteShelf)
	// Then
	if err != nil {
		t.Fatalf("nickelIsInCollection: %v", err)
	}
	if !got {
		t.Fatal("nickelIsInCollection = false, want true")
	}
}

func openNickelSchemaFixture(t *testing.T) *sql.DB {
	t.Helper()
	schema, err := os.ReadFile(filepath.Join("testdata", "nickel-schema-176.sql"))
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "KoboReader.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close Nickel test database: %v", err)
		}
	})
	execNickelSQL(t, db, string(schema))
	return db
}

func insertNickelContent(t *testing.T, db *sql.DB, outputDir, id string, status bookStatus) {
	t.Helper()
	execNickelSQL(t, db,
		`INSERT INTO content (ContentID, ContentType, MimeType, ReadStatus, ___UserID)
		 VALUES (?, ?, ?, ?, ?)`,
		nickelContentID(outputDir, id), nickelContentTypeBook, "application/epub+zip", status, "native-test-user",
	)
}

func insertNickelCollection(t *testing.T, db *sql.DB, outputDir, id, collection string) {
	t.Helper()
	execNickelSQL(t, db,
		`INSERT INTO Shelf (Id, InternalName, Name, _IsDeleted)
		 VALUES (?, ?, ?, 'false')`,
		"native-favourites", "native-favourites", collection,
	)
	execNickelSQL(t, db,
		`INSERT INTO ShelfContent (ShelfName, ContentId, _IsDeleted)
		 VALUES (?, ?, 'false')`,
		"native-favourites", nickelContentID(outputDir, id),
	)
}

func execNickelSQL(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}
