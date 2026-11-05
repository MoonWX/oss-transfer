package task

import (
	"path/filepath"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/execution"
)

func downloadSpec(ranged string) Spec {
	return Spec{Direction: "OSS_TO_LOCAL", Source: execution.Endpoint{Kind: "S3", Ref: "source", Bucket: "bucket"}, Destination: execution.Endpoint{Kind: "LOCAL"}, Nodes: []Node{{ID: "a", EnrollmentID: "ea", LocalRoot: "/mnt/a"}, {ID: "b", EnrollmentID: "eb", LocalRoot: "/mnt/b"}}, ConflictPolicy: "SKIP", DistributionMode: "BALANCED", RangedDownload: ranged}
}

func TestRangedDownloadChoiceIsValidated(t *testing.T) {
	for name, spec := range map[string]Spec{
		"off for an OSS destination": func() Spec { s := testSpec(); s.RangedDownload = "off"; return s }(),
		"unknown value":              downloadSpec("on"),
		"stored auto spelling":       downloadSpec("auto"),
	} {
		if err := validate(spec); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for _, spec := range []Spec{downloadSpec(""), downloadSpec("off"), testSpec()} {
		if err := validate(spec); err != nil {
			t.Errorf("valid spec %+v rejected: %v", spec.RangedDownload, err)
		}
	}
}

// The choice is stored with the task, reaches every child job, and survives
// a restart and a stop/resume (a new generation).
func TestRangedDownloadChoiceReachesEveryChildAndSurvivesResume(t *testing.T) {
	for _, ranged := range []string{"", "off"} {
		path := filepath.Join(t.TempDir(), "master.db")
		db, repo := openTest(t, path)
		p, _, err := repo.Create(downloadSpec(ranged), "request")
		if err != nil {
			t.Fatal(err)
		}
		for number := 1; number <= 2; number++ {
			child, err := repo.Assign(p.ID, 1, number, objects("key"+string(rune('0'+number))))
			if err != nil {
				t.Fatal(err)
			}
			if child.Job.RangedDownload != ranged || child.Job.Destination.Kind != "LOCAL" {
				t.Fatalf("child %d job carries %q, want %q", number, child.Job.RangedDownload, ranged)
			}
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		db, repo = openTest(t, path)
		if p, err = repo.Get(p.ID); err != nil || p.Spec.RangedDownload != ranged {
			t.Fatalf("stored choice %q after restart, want %q (%v)", p.Spec.RangedDownload, ranged, err)
		}
		if _, err := repo.Stop(p.ID); err != nil {
			t.Fatal(err)
		}
		if err := repo.Stopped(p.ID, 1); err != nil {
			t.Fatal(err)
		}
		if p, err = repo.Resume(p.ID); err != nil {
			t.Fatal(err)
		}
		prepareResume(t, repo, p)
		if err := repo.Resumed(p.ID, p.Generation); err != nil {
			t.Fatal(err)
		}
		children, err := repo.Children(p.ID, 0, 10)
		if err != nil || len(children) != 2 {
			t.Fatal(children, err)
		}
		for _, child := range children {
			if child.Job.Generation != p.Generation || child.Job.RangedDownload != ranged {
				t.Fatalf("resumed child %s: generation %d choice %q, want %q", child.Job.ChildID, child.Job.Generation, child.Job.RangedDownload, ranged)
			}
		}
		if p.Spec.RangedDownload != ranged {
			t.Fatal("resume changed the stored choice")
		}
		_ = db.Close()
	}
}
