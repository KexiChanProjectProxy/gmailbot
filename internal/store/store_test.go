package store

import (
	"context"
	"sync"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), t.TempDir()+"/t.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAccountsAndCursor(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if err := s.UpsertUser(ctx, 1, 100); err != nil {
		t.Fatal(err)
	}
	id, created, err := s.UpsertAccount(ctx, 1, "a@gmail.com", []byte("t1"), 50)
	if err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}
	if err := s.AdvanceHistory(ctx, id, 80); err != nil {
		t.Fatal(err)
	}
	if err := s.AdvanceHistory(ctx, id, 60); err != nil { // must not rewind
		t.Fatal(err)
	}
	id2, created, err := s.UpsertAccount(ctx, 1, "a@gmail.com", []byte("t2"), 10)
	if err != nil || created || id2 != id {
		t.Fatalf("relink: id=%d created=%v err=%v", id2, created, err)
	}
	a, _ := s.GetAccount(ctx, id)
	if a.HistoryID != 80 || string(a.TokenEnc) != "t2" {
		t.Errorf("account = %+v", a)
	}
	if err := s.DeleteUser(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if accs, _ := s.AllAccounts(ctx); len(accs) != 0 {
		t.Errorf("cascade failed: %v", accs)
	}
}

func TestInsertMessageDedupConcurrent(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	s.UpsertUser(ctx, 1, 100)
	a, _, _ := s.UpsertAccount(ctx, 1, "a@gmail.com", []byte("x"), 1)
	b, _, _ := s.UpsertAccount(ctx, 1, "b@gmail.com", []byte("x"), 1)

	var wg sync.WaitGroup
	results := make([]InsertResult, 2)
	for i, acc := range []int64{a, b} {
		wg.Add(1)
		go func(i int, acc int64) {
			defer wg.Done()
			r, err := s.InsertMessage(ctx, 1, "mid:same@x", Message{AccountID: acc, GmailID: "g", ChatID: 100,
				Attachments: []Attachment{{Idx: 0, Filename: "f", MIME: "text/plain", Size: 1}}})
			if err != nil {
				t.Error(err)
			}
			results[i] = r
		}(i, acc)
	}
	wg.Wait()
	winners := 0
	for _, r := range results {
		if !r.Inserted {
			t.Fatal("both copies must be stored")
		}
		if r.DupOf == 0 {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want 1 (%+v)", winners, results)
	}
	var win, loser InsertResult
	if results[0].DupOf == 0 {
		win, loser = results[0], results[1]
	} else {
		win, loser = results[1], results[0]
	}
	if loser.DupOf != win.ID {
		t.Errorf("loser points at %d, want %d", loser.DupOf, win.ID)
	}
	dups, _ := s.Duplicates(ctx, win.ID)
	if len(dups) != 1 || dups[0].ID != loser.ID {
		t.Errorf("duplicates = %+v", dups)
	}
	// Re-inserting the same gmail id is a no-op.
	r, _ := s.InsertMessage(ctx, 1, "mid:same@x", Message{AccountID: a, GmailID: "g", ChatID: 100})
	if r.Inserted {
		t.Error("duplicate insert for same account")
	}
	// A different user with the same key is independent.
	s.UpsertUser(ctx, 2, 200)
	c, _, _ := s.UpsertAccount(ctx, 2, "c@gmail.com", []byte("x"), 1)
	r, _ = s.InsertMessage(ctx, 2, "mid:same@x", Message{AccountID: c, GmailID: "g", ChatID: 200})
	if r.DupOf != 0 {
		t.Error("dedup leaked across users")
	}
	m, _ := s.GetMessage(ctx, win.ID)
	if len(m.Attachments) != 1 {
		t.Errorf("attachments not stored: %+v", m.Attachments)
	}
}

func TestOAuthStateSingleUse(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	s.PutOAuthState(ctx, OAuthState{State: "st", TgUserID: 1, ChatID: 1, Verifier: "v"}, time.Minute)
	if _, err := s.PeekOAuthState(ctx, "st"); err != nil {
		t.Fatal(err)
	}
	if st, err := s.TakeOAuthState(ctx, "st"); err != nil || st.Verifier != "v" {
		t.Fatalf("take: %+v %v", st, err)
	}
	if _, err := s.TakeOAuthState(ctx, "st"); err != ErrNotFound {
		t.Errorf("second take = %v", err)
	}
}

func TestCleanup(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	s.UpsertUser(ctx, 1, 100)
	a, _, _ := s.UpsertAccount(ctx, 1, "a@gmail.com", []byte("x"), 1)
	s.now = func() time.Time { return time.Now().Add(-8 * 24 * time.Hour) }
	s.InsertMessage(ctx, 1, "k-old", Message{AccountID: a, GmailID: "old", ChatID: 100})
	s.now = time.Now
	s.InsertMessage(ctx, 1, "k-new", Message{AccountID: a, GmailID: "new", ChatID: 100})
	n, err := s.Cleanup(ctx, 7*24*time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("cleanup n=%d err=%v", n, err)
	}
	if ok, _ := s.MessageExists(ctx, a, "new"); !ok {
		t.Error("fresh message deleted")
	}
}
