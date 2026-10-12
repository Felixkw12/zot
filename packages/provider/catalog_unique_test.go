package provider

import (
	"reflect"
	"testing"
)

func TestAppendCatalogPreservesFirstEntryAndProviderIdentity(t *testing.T) {
	original := Model{Provider: "first", ID: "shared", DisplayName: "Curated", ContextWindow: 100000, MaxOutput: 12000}
	other := Model{Provider: "second", ID: "shared", DisplayName: "Other provider"}
	newModel := Model{Provider: "first", ID: "new"}
	base := []Model{original}
	extra := []Model{{Provider: "first", ID: "shared", DisplayName: "Duplicate", ContextWindow: 200000}, other, newModel, newModel}
	got := appendCatalog(base, extra)
	if !reflect.DeepEqual(got, []Model{original, other, newModel}) {
		t.Fatalf("unexpected catalog composition: %+v", got)
	}
	if !reflect.DeepEqual(base, []Model{original}) || len(extra) != 4 {
		t.Fatal("catalog inputs were mutated")
	}
}

func TestStaticCatalogHasUniqueProviderModelPairs(t *testing.T) {
	seen := map[[2]string]bool{}
	for _, model := range Catalog {
		key := [2]string{model.Provider, model.ID}
		if seen[key] {
			t.Fatalf("duplicate catalog identity: %s/%s", model.Provider, model.ID)
		}
		seen[key] = true
	}
}
