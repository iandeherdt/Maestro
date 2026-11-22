package setup

import "testing"

func TestCheckWorkerRefusesTakenNames(t *testing.T) {
	st := &State{TakenWorkers: []string{"mac"}}
	if err := st.checkWorker("mac"); err == nil {
		t.Fatal("a taken name was accepted")
	}
	if err := st.checkWorker("mac-game"); err != nil {
		t.Fatal(err)
	}
	if err := st.checkWorker("Bad Name"); err == nil {
		t.Fatal("an invalid name was accepted")
	}
}
