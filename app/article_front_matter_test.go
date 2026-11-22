package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJournalDownloadProducesCitationInReviewAndAutomatic(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(fmt.Sprintf("automatic=%v", automatic), func(t *testing.T) {
			root, home := t.TempDir(), t.TempDir()
			source := filepath.Join(root, "10.1234-research.101-345.pdf")
			const target = "Example - 2023 - Mathematics Achievement in Primary Schools - Electronic Journal of Science Education.pdf"
			var content strings.Builder
			for i, line := range []string{
				"Electronic Journal of Science Education",
				"Vol. 17, Special Issue, October 2023, ISSN: 1111-1111",
				"Research Article", "Mathematics Achievement", "in Primary Schools", "Robin Example",
				"https://doi.org/10.1234/research.101", "Abstract This is an example.",
				"Corresponding author: Robin Example, University",
			} {
				fmt.Fprintf(&content, "BT /F1 12 Tf 72 %d Td (%s) Tj ET\n", 742-i*23, line)
			}
			writeTestPDFContent(t, source, content.String(), "<< /Author (Export Account) >>")
			a, err := newApp(home, filepath.Join(home, "engine"), filepath.Join(home, "models"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { a.s.close(); a.chat.cancel() }()
			a.online = nil
			if err := a.startUIOperation(root, true, false, automatic, false, false, nil, nil, false, scanRename); err != nil {
				t.Fatal(err)
			}
			state := waitForOperation(t, a)
			destination := filepath.Join(root, target)
			if !automatic {
				page, err := a.proposalPage(state.Job, 0)
				if err != nil || len(page.Rows) != 1 || page.Rows[0].To != destination {
					t.Fatalf("expected review proposal to %s, got %+v, %v; plan %+v", destination, page, err, a.s.plans)
				}
				if err := a.applySelected(state.Job, state.Revision, []int{page.Rows[0].Index}, false); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Stat(destination); err != nil {
				t.Fatalf("%v; plan %+v", err, a.s.plans)
			}
			if _, err := os.Stat(source); !os.IsNotExist(err) {
				t.Fatalf("source still exists: %v", err)
			}
			if automatic {
				if err := a.selectJob(state.Job); err != nil {
					t.Fatal(err)
				}
				page, err := a.reviewInspections(0, false, "auto")
				if err != nil || len(page.Rows) != 1 || page.Rows[0].AutomaticFrom != source || page.Rows[0].AutomaticTo != destination || page.Rows[0].AutomaticReason == "" {
					t.Fatalf("automatic receipt missing from saved run: %+v %v", page, err)
				}
			}
		})
	}
}
