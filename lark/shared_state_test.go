package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestMemoryCRUDAndAtomicCAS(t *testing.T) {
	t.Parallel()
	b, base := wsHarness(t, nil)
	const scope = "companion:cake"
	args := object{"scope": scope, "key": "round:1", "value": object{"move": 0}, "expected_version": 0}
	patch := object{"memory": args}
	if set := toolTest(t, base, "patch", patch, false)["memory"].(object)[scope].(object)["round:1"].(object); set["version"] != json.Number("1") {
		t.Fatal(set)
	}
	if conflict := toolTest(t, base, "patch", patch, true); conflict["error"] != "version_conflict" || conflict["version"] != json.Number("1") {
		t.Fatal(conflict)
	}
	a, z := make(chan error, 2), make(chan struct{})
	var workers sync.WaitGroup
	for i := 1; i <= 2; i++ {
		workers.Go(func() {
			<-z
			expected := uint64(1)
			_, err := b.db.setMemory(t.Context(), memoryWrite{scope, "round:1", fmt.Sprintf(`{"move":%d}`, i)}, &expected)
			a <- err
		})
	}
	close(z)
	workers.Wait()
	close(a)
	wins, conflicts := 0, 0
	for err := range a {
		var conflict *versionConflict
		switch {
		case err == nil:
			wins++
		case errors.As(err, &conflict) && conflict.version == 2:
			conflicts++
		default:
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatal(wins, conflicts)
	}
	if got := memoryScope(t, base, scope)["round:1"].(object); got["version"] != json.Number("2") || got["value"].(object)["move"] == json.Number("0") {
		t.Fatal(got)
	}
	toolTest(t, base, "patch", object{"memory": object{"scope": scope, "key": "round:1", "op": "delete"}}, false)
	if got := memoryScope(t, base, scope); len(got) != 0 {
		t.Fatal(got)
	}
	args["scope"] = "companion:Bad"
	toolTest(t, base, "patch", patch, true)
	args["scope"], args["value"] = "shared", strings.Repeat("x", 16384)
	toolTest(t, base, "patch", patch, true)
	for i := range 64 {
		if _, err := b.db.setMemory(t.Context(), memoryWrite{"shared", fmt.Sprint(i), "null"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.db.setMemory(t.Context(), memoryWrite{"shared", "extra", "null"}, nil); err == nil {
		t.Fatal("accepted 65th key")
	}
	if n, err := b.db.setMemory(t.Context(), memoryWrite{"shared", "0", "true"}, nil); err != nil || n != 2 {
		t.Fatal("cannot update full scope", n, err)
	}
	if got := toolTest(t, base, "patch", object{"memory": object{"op": "delete", "scope": "shared", "key": "0"}}, false)["memory"].(object)["shared"].(object)["0"].(object); got["deleted"] != true {
		t.Fatal(got)
	}
}

func memoryScope(t *testing.T, base, scope string) object {
	t.Helper()
	args := object{"include": []string{"memory"}, "memory_scopes": []string{scope}}
	return toolTest(t, base, "state", args, false)["memory"].(object)[scope].(object)
}

func TestPatchMemoryBatchRollback(t *testing.T) {
	t.Parallel()
	_, base := wsHarness(t, nil)
	edit := func(scope, key string, want int, value any) object {
		return object{"scope": scope, "key": key, "expected_version": want, "value": value}
	}
	batch := func(fail bool, ops ...object) object {
		return toolTest(t, base, "patch", object{"memory": ops}, fail)
	}
	batch(false, edit("shared", "a", 0, nil), edit("companion:cake", "b", 0, true))
	remove := object{"op": "delete", "scope": "companion:cake", "key": "b", "expected_version": 0}
	if conflict := batch(true, edit("shared", "a", 1, 3), remove); conflict["error"] != "version_conflict" || conflict["version"] != json.Number("1") {
		t.Fatal(conflict)
	}
	if a := memoryScope(t, base, "shared")["a"].(object); a["value"] != nil || a["version"] != json.Number("1") {
		t.Fatal("failed batch did not roll back", a)
	}
	remove["expected_version"] = 1
	result := batch(false, edit("shared", "a", 1, 3), remove)["memory"].(object)
	if a := result["shared"].(object)["a"].(object); a["value"] != json.Number("3") || a["version"] != json.Number("2") {
		t.Fatal(result)
	}
	if len(memoryScope(t, base, "companion:cake")) != 0 {
		t.Fatal("batch delete did not commit")
	}
}
