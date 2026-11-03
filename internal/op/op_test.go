package op

import (
	"context"
	"errors"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

type in struct {
	Name  string `json:"name"`
	Count int    `json:"count,omitempty"`
}

type out struct {
	Greeting string `json:"greeting"`
}

func TestCallChecksInputAndOutput(t *testing.T) {
	greet := Define("greet", "Greets.", "x greet --name a", Annotations{ReadOnly: true},
		func(_ context.Context, i in) (out, error) { return out{Greeting: "hello " + i.Name}, nil })
	got, err := greet.Call(context.Background(), []byte(`{"name":"a"}`))
	if err != nil || string(got) != `{"greeting":"hello a"}` {
		t.Fatalf("got %s, %v", got, err)
	}
	for _, input := range []string{`{}`, `{"name":1}`, `{"name":"a","extra":true}`, `not json`} {
		_, err := greet.Call(context.Background(), []byte(input))
		if AsError(err).Code != CodeInvalidInput {
			t.Errorf("input %s: error %v, want invalid input", input, err)
		}
	}
}

func TestCallRejectsOutputOutsideItsSchema(t *testing.T) {
	type loose struct {
		Value any `json:"value"`
	}
	o := Define("bad", "Returns a number where a string is declared.", "", Annotations{},
		func(context.Context, in) (loose, error) { return loose{Value: 1}, nil })
	declared, err := jsonschema.For[struct {
		Value string `json:"value"`
	}](nil)
	if err != nil {
		t.Fatal(err)
	}
	if o.output, err = declared.Resolve(nil); err != nil {
		t.Fatal(err)
	}
	_, err = o.Call(context.Background(), []byte(`{"name":"a"}`))
	if AsError(err).Code != CodeInternal {
		t.Errorf("error %v, want internal", err)
	}
}

func TestErrorsKeepTheirCode(t *testing.T) {
	o := Define("fail", "Fails.", "", Annotations{},
		func(context.Context, in) (out, error) { return out{}, Errorf(CodeUnavailable, "down") })
	_, err := o.Call(context.Background(), []byte(`{"name":"a"}`))
	if e := AsError(err); e.Code != CodeUnavailable || e.Message != "down" {
		t.Errorf("got %+v", e)
	}
	if AsError(errors.New("plain")).Code != CodeInternal {
		t.Error("an error without a code is not internal")
	}
}
