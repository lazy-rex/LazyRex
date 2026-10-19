package main

import (
	"context"
	"testing"
)

func TestLegacyExitDoesNotSuppressCurrentTab(t *testing.T) {
	h, _ := workerTestHub(t)
	if _, err := h.db.Exec(uiSchema); err != nil {
		t.Fatal(err)
	}
	h.ui = &uiState{kick: make(chan struct{}, 1), sess: map[string]string{"old-session": "messaging", "new-session": uiSessionKey}}
	if _, err := h.db.Exec(`INSERT INTO ui_tabs (plugin,tab,title,route,pos,crashes) VALUES ('messaging','home','Messages','/',0,2);
 INSERT INTO views (view_id,plugin,tab,route,status,session_id,block_id) VALUES ('old-view','messaging','home','/','open','old-session','old-block');`); err != nil {
		t.Fatal(err)
	}
	if !h.uiChildExited(context.Background(), "old-block") {
		t.Fatal("legacy event not handled")
	}
	var suppressed, crashes int
	if err := h.db.QueryRow(`SELECT suppressed,crashes FROM ui_tabs WHERE plugin='messaging' AND tab='home'`).Scan(&suppressed, &crashes); err != nil {
		t.Fatal(err)
	}
	if suppressed != 0 || crashes != 2 {
		t.Fatalf("legacy exit changed current tab: suppressed=%d crashes=%d", suppressed, crashes)
	}
}
