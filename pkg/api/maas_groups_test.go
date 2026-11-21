package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// stubMaaSGroups replaces the group check and records the names it got.
func stubMaaSGroups(t *testing.T) *[][]string {
	t.Helper()
	orig := checkMaaSGroups
	t.Cleanup(func() { checkMaaSGroups = orig })
	var calls [][]string
	checkMaaSGroups = func(_ *cluster.Client, names []string) ([]types.MaaSGroupProblem, string) {
		calls = append(calls, names)
		if names[0] == "system:authenticated" {
			return nil, "system:authenticated is every signed-in user: choose Everyone signed in"
		}
		return []types.MaaSGroupProblem{{Name: names[0], Missing: true, Message: names[0] + " is not a group on this cluster."}}, ""
	}
	return &calls
}

func getMaaSGroups(t testing.TB, query, token string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	Register(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, postJSONMethod(t, http.MethodGet, "/api/resources/maas/groups"+query, token, ""))
	return w
}

func TestMaaSGroups(t *testing.T) {
	t.Run("a cluster-admin gets the problems of the names typed", func(t *testing.T) {
		setupDevMode(t)
		allowMutations(t)
		calls := stubMaaSGroups(t)
		w := getMaaSGroups(t, "?name=nobody-yet&name=%2Fadmins", "user:admin")
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		if len(*calls) != 1 || !reflect.DeepEqual((*calls)[0], []string{"nobody-yet", "/admins"}) {
			t.Fatalf("checked %v", *calls)
		}
		var got []types.MaaSGroupProblem
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got) != 1 || got[0].Name != "nobody-yet" || !got[0].Missing {
			t.Fatalf("body=%s err=%v", w.Body.String(), err)
		}
	})

	t.Run("anyone else gets 403 and nothing is checked", func(t *testing.T) {
		setupDevMode(t)
		mutationPermission = func(context.Context, string) (bool, error) { return false, nil }
		calls := stubMaaSGroups(t)
		w := getMaaSGroups(t, "?name=ml-team", "user:reader")
		if body := decodeError(t, w); w.Code != http.StatusForbidden || body["errorCode"] != adminOnlyCode || len(*calls) != 0 {
			t.Fatalf("status=%d calls=%v body=%s", w.Code, *calls, w.Body.String())
		}
	})

	t.Run("no names, too many, or one Publish refuses: 400", func(t *testing.T) {
		setupDevMode(t)
		allowMutations(t)
		stubMaaSGroups(t)
		many := ""
		for range 21 {
			many += "&name=g"
		}
		for _, q := range []string{"", "?" + many[1:], "?name=system:authenticated"} {
			w := getMaaSGroups(t, q, "user:admin")
			if body := decodeError(t, w); w.Code != http.StatusBadRequest || body["errorCode"] != "validation" {
				t.Errorf("%q: status=%d body=%s", q, w.Code, w.Body.String())
			}
		}
	})
}
