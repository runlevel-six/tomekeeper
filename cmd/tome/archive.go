package main

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// archiveCmd reports on the archive.
//
// The measurement exists because the acceptance criterion asks for storage to
// be measured against real articles and recorded. A number from someone's
// actual reading is worth more than any estimate, and this is how they get it.
func archiveCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		archiveUsage(stderr)
		return exitUsage
	}

	switch args[0] {
	case "stats":
		if len(args) > 1 {
			return usageError(stderr, "archive stats", args[1])
		}
		return archiveStats(stdout, stderr)
	default:
		fmt.Fprintf(stderr, "tome archive: unknown action %q\n\n", args[0])
		archiveUsage(stderr)
		return exitUsage
	}
}

func archiveUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: tome archive <action>

Actions:
  stats    Report what the archive holds and what it costs on disk
`)
}

func archiveStats(stdout, stderr io.Writer) int {
	return withStore(stderr, func(s *store.Store) int {
		ctx, stop := signalContext()
		defer stop()

		st, err := s.System().Stats(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "tome archive: %v\n", err)
			return exitFailure
		}

		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)

		fmt.Fprintf(tw, "articles\t%d\n", st.Articles)
		fmt.Fprintf(tw, "  fetched\t%d\n", st.ArticlesFetched)
		fmt.Fprintf(tw, "  with a body\t%d\n", st.Bodies)
		fmt.Fprintf(tw, "  partial images\t%d\n", st.AssetsPartial)
		fmt.Fprintf(tw, "body text\t%s\n", humanBytes(st.BodyBytes))
		fmt.Fprintln(tw)
		fmt.Fprintf(tw, "images stored\t%d\n", st.Assets)
		fmt.Fprintf(tw, "image references\t%d\n", st.AssetLinks)
		fmt.Fprintf(tw, "image bytes\t%s\n", humanBytes(st.AssetBytes))

		// The deduplication win, stated rather than implied. An image used by
		// ten articles is stored once, and this is where that shows up.
		if st.Assets > 0 && st.AssetLinks > st.Assets {
			saved := st.AssetLinks - st.Assets
			average := st.AssetBytes / st.Assets
			fmt.Fprintf(tw, "  deduplicated\t%d references, about %s not stored twice\n",
				saved, humanBytes(saved*average))
		}

		if st.Bodies > 0 {
			fmt.Fprintln(tw)
			perArticle := (st.BodyBytes + st.AssetBytes) / st.Bodies
			fmt.Fprintf(tw, "per article\t%s (body and images; excludes raw pages on disk)\n",
				humanBytes(perArticle))
			fmt.Fprintf(tw, "per 1,000 articles\t%s\n", humanBytes(perArticle*1000))
		}

		_ = tw.Flush()

		// Raw pages are on the filesystem rather than in the database, so the
		// honest way to size them is to ask the filesystem.
		fmt.Fprintf(stdout, "\nRaw pages are not counted above. Measure them with:\n")
		fmt.Fprintf(stdout, "  du -sh $TOME_BLOB_ROOT/articles\n")
		fmt.Fprintf(stdout, "  du -sh $TOME_BLOB_ROOT/assets\n")
		return exitOK
	})
}

// humanBytes formats a byte count the way a person would read it.
func humanBytes(n int64) string {
	const unit = 1024

	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	div, exp := int64(unit), 0
	for size := n / unit; size >= unit && exp < 4; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}
