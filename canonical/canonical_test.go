package canonical

import (
	"net/http"
	"testing"
)

func TestHashStableUnderHeaderOrder(t *testing.T) {
	opts := Options{Namespace: "/openai"}
	h1 := http.Header{}
	h1.Add("Content-Type", "application/json")
	h1.Add("X-Foo", "1")
	h1.Add("X-Bar", "2")

	h2 := http.Header{}
	h2.Add("X-Bar", "2")
	h2.Add("X-Foo", "1")
	h2.Add("content-type", "application/json")

	body := []byte(`{"a":1,"b":2}`)
	_, hash1 := Canonicalize("POST", "/v1/x", "", h1, body, opts)
	_, hash2 := Canonicalize("POST", "/v1/x", "", h2, body, opts)
	if hash1 != hash2 {
		t.Fatalf("hashes differ across header order: %s vs %s", hash1, hash2)
	}
}

func TestHashStableUnderJSONKeyOrder(t *testing.T) {
	opts := Options{Namespace: "/openai"}
	h := http.Header{"Content-Type": []string{"application/json"}}
	b1 := []byte(`{"a":1,"b":[3,{"x":1,"y":2}]}`)
	b2 := []byte(`{"b":[3,{"y":2,"x":1}],"a":1}`)
	_, h1 := Canonicalize("POST", "/v1/x", "", h, b1, opts)
	_, h2 := Canonicalize("POST", "/v1/x", "", h, b2, opts)
	if h1 != h2 {
		t.Fatalf("json key order changed hash: %s vs %s", h1, h2)
	}
}

func TestHashStableUnderQueryOrder(t *testing.T) {
	opts := Options{Namespace: "/openai"}
	h := http.Header{}
	_, a := Canonicalize("GET", "/v1/x", "b=2&a=1", h, nil, opts)
	_, b := Canonicalize("GET", "/v1/x", "a=1&b=2", h, nil, opts)
	if a != b {
		t.Fatalf("query order changed hash: %s vs %s", a, b)
	}
}

func TestQueryBlacklist(t *testing.T) {
	opts := Options{Namespace: "/openai", QueryBlacklist: []string{"_t"}}
	h := http.Header{}
	_, a := Canonicalize("GET", "/x", "a=1&_t=999", h, nil, opts)
	_, b := Canonicalize("GET", "/x", "a=1", h, nil, opts)
	if a != b {
		t.Fatalf("blacklisted param affected hash")
	}
}

func TestNamespaceIsolatesCollisions(t *testing.T) {
	h := http.Header{}
	_, a := Canonicalize("GET", "/v1/x", "", h, nil, Options{Namespace: "/openai"})
	_, b := Canonicalize("GET", "/v1/x", "", h, nil, Options{Namespace: "/stripe"})
	if a == b {
		t.Fatalf("namespaces collided")
	}
}

func TestAuthorizationRespected(t *testing.T) {
	h1 := http.Header{"Authorization": []string{"Bearer alice"}}
	h2 := http.Header{"Authorization": []string{"Bearer bob"}}

	// Default: auth excluded → same hash
	_, a := Canonicalize("GET", "/x", "", h1, nil, Options{Namespace: "/n"})
	_, b := Canonicalize("GET", "/x", "", h2, nil, Options{Namespace: "/n"})
	if a != b {
		t.Fatalf("auth header should be excluded by default")
	}

	// Opt-in: auth included → different hashes
	_, a = Canonicalize("GET", "/x", "", h1, nil, Options{Namespace: "/n", IncludeAuth: true})
	_, b = Canonicalize("GET", "/x", "", h2, nil, Options{Namespace: "/n", IncludeAuth: true})
	if a == b {
		t.Fatalf("auth header should differentiate when IncludeAuth=true")
	}
}
