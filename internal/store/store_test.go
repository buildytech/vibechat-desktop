package store

import "testing"

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := Project{ID: "p1", Name: "Demo", CreatedAt: Now(), UpdatedAt: Now()}
	if err := s.PutProject(p); err != nil {
		t.Fatal(err)
	}
	_ = s.PutSession(Session{ID: "s1", ProjectID: "p1", CreatedAt: Now()})
	a, _ := s.Append(Message{SessionID: "s1", Role: "user", Content: "hi", ClientID: "c1"})
	b, _ := s.Append(Message{SessionID: "s1", Role: "user", Content: "hi", ClientID: "c1"})
	if a.ID != b.ID || a.Seq != 1 {
		t.Fatal("clientMsgId not idempotent")
	}
	_, _ = s.Append(Message{SessionID: "s1", Role: "assistant", Content: "yo", RunID: "r1"})
	if err := s.SetFeedback("s1", "r1", "up"); err != nil {
		t.Fatal(err)
	}
	s2, _ := Open(dir)
	msgs, _ := s2.Messages("s1", 1)
	if len(msgs) != 1 || msgs[0].Feedback != "up" || msgs[0].Seq != 2 {
		t.Fatalf("%+v", msgs)
	}
	if _, err := s2.Session("other", "s1"); err != ErrNotFound {
		t.Fatal("session leaked across projects")
	}
	_ = s2.DeleteProject("p1")
	if len(s2.Projects()) != 0 || len(s2.Sessions("p1")) != 0 {
		t.Fatal("delete")
	}
}
