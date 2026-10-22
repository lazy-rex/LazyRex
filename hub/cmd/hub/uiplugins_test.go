package main

import (
	"context"
	"reflect"
	"testing"
)

// uiPlugins returns the plugins with declared tabs and the plugins with a live tab view (the rows
// that loadViews reads), the home plugin first. A home plugin that declares no tabs, with an open
// view in a legacy session, still gets its pass, so that view retires (reconcile, step 1).
func TestUIPluginsIncludesLiveTabViews(t *testing.T) {
	h, _ := workerTestHub(t)
	if _, err := h.db.Exec(uiSchema); err != nil {
		t.Fatal(err)
	}
	if err := formsStart(context.Background(), h.db); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`INSERT INTO ui_tabs (plugin,tab,title,route,pos) VALUES ('zeta','home','Zeta','/',0);
 INSERT INTO views (view_id,plugin,tab,route,status,session_id,block_id) VALUES
  ('home-legacy','lazyrex','home','/','open','legacy-session','b1'),
  ('alpha-closed','alpha','home','/','closed','s','b2'),
  ('beta-form','beta','` + formTab + `','/forms/x','open','s','b3'),
  ('gamma-sending','gamma','home','/','sending','s','');`); err != nil {
		t.Fatal(err)
	}
	got, err := h.uiPlugins(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"lazyrex", "gamma", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("uiPlugins = %v, want %v", got, want)
	}
}
