package task

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

func assignmentPage() []Assignment {
	page := make([]Assignment, 16)
	for i := range page {
		page[i] = Assignment{Number: i + 1, Objects: objects(fmt.Sprintf("object-%d", i))}
	}
	return page
}

func TestAssignmentPageMatchesSingleAssignmentsAndSurvivesRestart(t *testing.T) {
	var reference []Child
	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprintf("batch=%v", batch), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "master.db")
			db, repo := openTest(t, path)
			defer func() { _ = db.Close() }()
			p, _, err := repo.Create(testSpec(), "assignment-page")
			if err != nil {
				t.Fatal(err)
			}
			page := assignmentPage()
			before := durableTransactionID(t, path)
			var children []Child
			if batch {
				children, err = repo.AssignPage(p.ID, p.Generation, page)
			} else {
				for _, assignment := range page {
					child, assignErr := repo.Assign(p.ID, p.Generation, assignment.Number, assignment.Objects)
					if assignErr != nil {
						t.Fatal(assignErr)
					}
					children = append(children, child)
				}
				reference = children
			}
			if err != nil || !reflect.DeepEqual(children, reference) {
				t.Fatalf("page changed node assignment or job definition: %v", err)
			}
			want := 16
			if batch {
				want = 1
			}
			if commits := durableTransactionID(t, path) - before; commits != want {
				t.Fatalf("assignment commits=%d want=%d", commits, want)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, repo = openTest(t, path)
			outstanding, err := repo.Outstanding(p.ID, 0, 32)
			if err != nil || !reflect.DeepEqual(outstanding, reference) {
				t.Fatalf("undelivered assignments were lost across restart: %v", err)
			}
			current, err := repo.Get(p.ID)
			if err != nil || current.DispatchedThrough != 16 || current.ChildCount != 16 || current.DispatchedObjects != 16 || current.DispatchedBytes != 16*1024 {
				t.Fatalf("page counters changed: %+v %v", current, err)
			}
			assertNoTaskCommit(t, path, func() error {
				replayed, err := repo.AssignPage(p.ID, p.Generation, page)
				if err == nil && !reflect.DeepEqual(replayed, reference) {
					t.Fatal("replayed page changed assignment")
				}
				return err
			})
		})
	}
}

func TestAssignmentPageRollsBackLoadCountersAndAllChildrenOnFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.db")
	db, repo := openTest(t, path)
	defer db.Close()
	p, _, err := repo.Create(testSpec(), "assignment-page")
	if err != nil {
		t.Fatal(err)
	}
	page := assignmentPage()
	page[15].Objects[0].Size = -1 // Fail after the first fifteen assignments.
	before := databaseDigest(t, path)
	if children, err := repo.AssignPage(p.ID, p.Generation, page); err == nil || len(children) != 0 {
		t.Fatal("failed assignment page returned usable jobs", err)
	}
	if databaseDigest(t, path) != before {
		t.Fatal("failed assignment page committed a prefix")
	}
	page[15].Objects[0].Size = 1024
	children, err := repo.AssignPage(p.ID, p.Generation, page)
	if err != nil || len(children) != 16 || children[0].Node.ID != "a" || children[1].Node.ID != "b" {
		t.Fatal("failed page changed retry distribution", err)
	}
	if _, err := repo.Stop(p.ID); err != nil {
		t.Fatal(err)
	}
	before = databaseDigest(t, path)
	if _, err := repo.AssignPage(p.ID, p.Generation, page); !errors.Is(err, ErrState) {
		t.Fatal("stopped parent accepted a dispatch page", err)
	}
	if _, err := repo.AssignPage(p.ID, p.Generation+1, page); !errors.Is(err, ErrGeneration) {
		t.Fatal("wrong generation accepted a dispatch page", err)
	}
	if databaseDigest(t, path) != before {
		t.Fatal("fenced page changed database")
	}
}
