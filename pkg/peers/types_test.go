package peers

import "testing"

func TestParse_ValidAndInvalidMix(t *testing.T) {
	const payload = `[
		{"name": "good-a", "public_key": "5nugtxtT5TAp2VF5Gns5zpHQRT0hQuF/ygU5pNGdqEw=", "allowed_ips": ["10.0.0.1/32"], "endpoint": "1.2.3.4:51820"},
		{"name": "bad-key", "public_key": "not-base64", "allowed_ips": ["10.0.0.2/32"]},
		{"name": "bad-cidr", "public_key": "aecnfF7mIaRnImSJChoUWnidkQN5h2bPcKdjbjdLBVQ=", "allowed_ips": ["not-a-cidr"]},
		{"name": "no-endpoint", "public_key": "CPoM5y/cJPS9mc52uim6Q9l3xKw4Au22hWJ5+Sgwo2A=", "allowed_ips": ["10.0.0.3/32"], "endpoint": null}
	]`

	valid, invalid, err := Parse("test-secret", []byte(payload))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(valid) != 2 {
		t.Fatalf("valid = %d entries, want 2: %+v", len(valid), valid)
	}
	if valid[0].Name != "good-a" || valid[0].Endpoint == nil {
		t.Errorf("valid[0] = %+v, want good-a with a resolved endpoint", valid[0])
	}
	if valid[1].Name != "no-endpoint" || valid[1].Endpoint != nil {
		t.Errorf("valid[1] = %+v, want no-endpoint with a nil endpoint", valid[1])
	}

	if len(invalid) != 2 {
		t.Fatalf("invalid = %d entries, want 2: %+v", len(invalid), invalid)
	}
	for _, inv := range invalid {
		if inv.Source != "test-secret" {
			t.Errorf("invalid entry source = %q, want test-secret", inv.Source)
		}
	}
}

func TestParse_MalformedDocument(t *testing.T) {
	if _, _, err := Parse("test-secret", []byte(`not json`)); err == nil {
		t.Fatal("Parse() error = nil, want error for malformed top-level document")
	}
}

func TestParse_Empty(t *testing.T) {
	valid, invalid, err := Parse("test-secret", []byte(`[]`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(valid) != 0 || len(invalid) != 0 {
		t.Errorf("valid = %v, invalid = %v, want both empty", valid, invalid)
	}
}
