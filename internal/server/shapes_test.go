package server

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"testing"
	"time"
)

// shapesFixture is the API's JSON shapes the console's dev mock is checked
// against (console/src/mockPeople.test.ts): for each type, the keys a zero
// value marshals (sent even when empty) and a filled one (every key it may
// send). LUX_UPDATE_SHAPES=1 rewrites it; otherwise a stale fixture fails.
const shapesFixture = "../../console/src/api/testdata/shapes.json"

type apiShape struct {
	Zero   []string `json:"zero"`
	Filled []string `json:"filled"`
}

// shapeTypes: the people API's types, by the name the console's types use.
var shapeTypes = map[string]any{
	"Person": Person{}, "Me": Me{}, "Member": Member{}, "MembershipRef": MembershipRef{}, "Whoami": Whoami{},
	"ServiceAccount": ServiceAccount{}, "ApiKey": ApiKey{}, "Invitation": Invitation{}, "Tenant": Tenant{},
	"TenantOwner": TenantOwner{}, "CostSummaryRow": CostSummaryRow{}, "Owner": Owner{},
}

// fill sets every exported field of v (recursively) to a non-empty value.
func fill(v reflect.Value, depth int) {
	if depth > 4 {
		return
	}
	switch v.Kind() {
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			v.Set(reflect.ValueOf(time.Unix(1, 0).UTC()))
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if f := v.Field(i); f.CanSet() {
				fill(f, depth+1)
			}
		}
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fill(p.Elem(), depth+1)
		v.Set(p)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fill(s.Index(0), depth+1)
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		fill(k, depth+1)
		fill(e, depth+1)
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	case reflect.Interface:
		v.Set(reflect.ValueOf("x"))
	}
}

func topKeys(t *testing.T, v any) []string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func TestAPIShapesFixture(t *testing.T) {
	got := map[string]apiShape{}
	for name, zero := range shapeTypes {
		filled := reflect.New(reflect.TypeOf(zero)).Elem()
		fill(filled, 0)
		got[name] = apiShape{Zero: topKeys(t, zero), Filled: topKeys(t, filled.Interface())}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(got); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LUX_UPDATE_SHAPES") == "1" {
		if err := os.WriteFile(shapesFixture, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	have, err := os.ReadFile(shapesFixture)
	if err != nil {
		t.Fatalf("%v: run LUX_UPDATE_SHAPES=1 go test ./internal/server -run TestAPIShapesFixture", err)
	}
	if !bytes.Equal(have, buf.Bytes()) {
		t.Fatalf("%s is stale: the API's JSON shapes changed. Run LUX_UPDATE_SHAPES=1 go test ./internal/server -run TestAPIShapesFixture, then make the console's mock (mockPeople.test.ts) match.\nwant:\n%s", shapesFixture, buf.String())
	}
}
