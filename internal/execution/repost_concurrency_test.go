package execution

import (
	"context"
	"sync"
	"testing"
)

func TestRepostConcurrentPollAndRecoveryDoNotDuplicate(t *testing.T) {
	f := newRepostFixture(t)
	f.listStatus = 503
	f.poll(t) // Reconciliation failure remains local to the repost.
	f.listStatus = 0
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(poll bool) {
			defer wg.Done()
			var err error
			if poll {
				err = f.e.pollCommands(context.Background())
			} else {
				err = f.e.recoverNotifications(context.Background())
			}
			if err != nil {
				t.Error(err)
			}
		}(i%2 == 0)
	}
	wg.Wait()
	if f.posts != 1 {
		t.Fatalf("concurrent outbox recovery duplicated a POST: %d", f.posts)
	}
}
