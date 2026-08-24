package backup_test

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/backup"
)

// Scale, with the shape a real archive has: long slugged directories, three files per
// article, and hashes recorded for two of the three.
func TestVerifyAtScale(t *testing.T) {
	fx := newArchiveFixture(t)
	ctx := t.Context()

	// 300 more articles, each with a raw page recorded in the database and two derived
	// files that are not.
	for i := 0; i < 300; i++ {
		slug := fmt.Sprintf("a-rather-long-slugged-title-of-the-kind-real-articles-have-%04d-abcdef12", i)
		rel := "articles/2026/08/" + slug + "/raw.html.gz"
		body := fmt.Sprintf("stored page %d", i)
		fx.write(t, rel, body)
		fx.write(t, "articles/2026/08/"+slug+"/index.html", "<html/>")
		fx.write(t, "articles/2026/08/"+slug+"/meta.json", "{}")

		id, _, err := fx.store.UpsertArticle(ctx, mkParams(slug))
		if err != nil {
			t.Fatal(err)
		}
		if err := fx.recordRaw(ctx, id, rel, body); err != nil {
			t.Fatal(err)
		}
	}

	// One article stored the way the fetcher really stores one: the file on disk is
	// gzip(page) while articles.raw_blob_sha is the SHA of the *page*. Without this,
	// every fixture here writes bytes whose hash the test itself computed — which is
	// exactly why format 1's mistake survived a green suite and was found only by
	// pointing it at 10,774 real files.
	plain := "the page as it was fetched, before compression"
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write([]byte(plain)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	stored := "articles/2026/08/stored-gzipped-abcdef12/raw.html.gz"
	fx.write(t, stored, gz.String())
	id, _, err := fx.store.UpsertArticle(ctx, mkParams("stored-gzipped-abcdef12"))
	if err != nil {
		t.Fatal(err)
	}
	// The SHA of the plaintext, which is what the fetch worker records.
	if err := fx.recordRaw(ctx, id, stored, plain); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	result, err := backup.Write(ctx, fx.pool, &buf, backup.Options{BlobRoot: fx.root, Version: "test"})
	if err != nil {
		t.Fatalf("Write() = %v", err)
	}
	t.Logf("wrote %d files, %d missing", len(result.Manifest.Files), len(result.Manifest.Missing))

	report, err := backup.Verify(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("Verify() = %v", err)
	}
	t.Logf("verified=%d absent=%d corrupt=%d extra=%d",
		report.Verified, len(report.Absent), len(report.Corrupt), len(report.Extra))
	if len(report.Absent) > 0 {
		t.Errorf("absent entries, first few: %v", report.Absent[:min(5, len(report.Absent))])
	}
	if len(report.Corrupt) > 0 {
		t.Errorf("%d entries did not match their recorded hash, first few: %v\n"+
			"this is the bug format 2 exists for: the manifest must record the hash of the "+
			"bytes as stored, not the database's hash of what was originally fetched",
			len(report.Corrupt), report.Corrupt[:min(5, len(report.Corrupt))])
	}
	if !report.OK() {
		t.Fatal("an archive this program just wrote does not verify")
	}

	// Every entry is verifiable now. A gzipped page and a transcoded image have no
	// recorded hash that matches their stored bytes, which is what format 1 got wrong,
	// so the count has to cover the whole tree rather than a third of it.
	if report.Verified != len(result.Manifest.Files)+len(result.Manifest.Tables) {
		t.Errorf("verified %d of %d entries; every file and table should be checkable",
			report.Verified, len(result.Manifest.Files)+len(result.Manifest.Tables))
	}
}
