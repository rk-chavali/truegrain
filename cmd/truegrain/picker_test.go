package main

import (
	"strings"
	"testing"

	"github.com/rk-chavali/truegrain/internal/introspect"
)

// Choosing a dataset.
//
// Typing a name means knowing it, and the person running init is often the one
// who does not, so the question is a menu. The parsing is tested apart from the
// prompt because it is the half that can go quietly wrong: reading "1,3" as a
// single dataset named "1,3" produces a confusing error about a dataset that
// does not exist, rather than the two the person asked for.

var listed = []introspect.DatasetSummary{
	{Name: "analytics", Tables: 12},
	{Name: "retail_star_schema", Tables: 7},
	{Name: "truegrain_demo", Tables: 4},
}

func TestANumberPicksTheDatasetBesideIt(t *testing.T) {
	got, err := resolveDatasets("2", listed)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "retail_star_schema" {
		t.Errorf("got %v, want [retail_star_schema]", got)
	}
}

func TestADatasetNameWorksToo(t *testing.T) {
	// Somebody who already knows the dataset should not have to read the list.
	for _, typed := range []string{"truegrain_demo", "  truegrain_demo  ", "TRUEGRAIN_DEMO"} {
		got, err := resolveDatasets(typed, listed)
		if err != nil {
			t.Fatalf("%q: %v", typed, err)
		}
		if len(got) != 1 || got[0] != "truegrain_demo" {
			t.Errorf("%q gave %v", typed, got)
		}
	}
}

func TestSeveralNumbersPickSeveralDatasets(t *testing.T) {
	got, err := resolveDatasets("1, 3", listed)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "analytics" || got[1] != "truegrain_demo" {
		t.Fatalf("got %v, want [analytics truegrain_demo]", got)
	}
}

// TestTheLastEntryTakesEverything, which is the entry printed after the real
// datasets and is the reason somebody opens this menu at all on a project they
// do not know.
func TestTheLastEntryTakesEverything(t *testing.T) {
	for _, typed := range []string{"4", "all", "ALL"} {
		got, err := resolveDatasets(typed, listed)
		if err != nil {
			t.Fatalf("%q: %v", typed, err)
		}
		if len(got) != len(listed) {
			t.Errorf("%q chose %d of %d datasets", typed, len(got), len(listed))
		}
	}
}

func TestTheSameDatasetTwiceIsReadOnce(t *testing.T) {
	// Writing one namespace twice in a run would report it as written twice
	// and regenerate it on top of itself.
	got, err := resolveDatasets("2,retail_star_schema,2", listed)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("got %v, want one dataset", got)
	}
}

// TestSomethingThatIsNotThereIsRefusedByName.
//
// A number past the end and a name nobody listed are different mistakes and
// deserve different messages: one is a misread menu, the other a typo.
func TestSomethingThatIsNotThereIsRefusedByName(t *testing.T) {
	if _, err := resolveDatasets("9", listed); err == nil {
		t.Error("a number past the end of the list was accepted")
	} else if !strings.Contains(err.Error(), "3 datasets") {
		t.Errorf("the message should say how many there are: %v", err)
	}

	if _, err := resolveDatasets("warehouse", listed); err == nil {
		t.Error("a dataset that is not in the project was accepted")
	} else if !strings.Contains(err.Error(), "warehouse") {
		t.Errorf("the message should name what was typed: %v", err)
	}

	if _, err := resolveDatasets("   ", listed); err == nil {
		t.Error("an empty answer was accepted")
	}
}
