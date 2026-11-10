package googlephotos

import (
	"encoding/json"
	"testing"
)

func contributorObject(id, name string) []interface{} {
	obj := make([]interface{}, 18)
	obj[0] = id
	obj[11] = []interface{}{name, 1, nil, name}
	return obj
}

// contributorListEntry matches the real data[3][21] layout: the ID pair is at
// index 0 and the display-name details are at index 3.
func contributorListEntry(id, name string) []interface{} {
	obj := make([]interface{}, 12)
	obj[0] = []interface{}{id, "user-" + id}
	obj[3] = []interface{}{name, 1, nil, name}
	obj[11] = []interface{}{"https://example.test/avatar"}
	return obj
}

func photoItem(id, contributorID string) []interface{} {
	item := make([]interface{}, 10)
	item[0] = id
	item[1] = []interface{}{"https://example.test/" + id, 100, 200}
	if contributorID != "" {
		item[6] = []interface{}{contributorID}
	}
	item[9] = map[string]interface{}{}
	return item
}

func albumData(contributors []interface{}, items []interface{}) []interface{} {
	meta := make([]interface{}, 22)
	meta[0] = "album-id"
	meta[1] = "Album Title"
	meta[9] = contributors[0]
	meta[10] = []interface{}{"Owner Name", 1, nil, "Owner"}
	meta[21] = contributors
	return []interface{}{nil, items, "", meta}
}

func TestParseContributors(t *testing.T) {
	alice := contributorObject("alice-id", "Alice")
	bob := contributorObject("bob-id", "Bob")
	data := albumData([]interface{}{alice, bob}, nil)

	got := parseContributors(data)
	if got["alice-id"] != "Alice" || got["bob-id"] != "Bob" {
		t.Fatalf("unexpected contributors: %#v", got)
	}
}

func TestParseContributorsListLayout(t *testing.T) {
	alice := contributorListEntry("alice-id", "Alice")
	bob := contributorListEntry("bob-id", "Bob")
	meta := make([]interface{}, 22)
	meta[9] = alice
	meta[10] = []interface{}{"Alice", 1, nil, "Alice"}
	meta[21] = []interface{}{alice, bob}
	data := []interface{}{nil, nil, "", meta}

	got := parseContributors(data)
	if got["alice-id"] != "Alice" || got["bob-id"] != "Bob" {
		t.Fatalf("unexpected contributors from list layout: %#v", got)
	}
}

func TestParseContributorsFallsBackToOwner(t *testing.T) {
	owner := contributorObject("owner-id", "Owner Name")
	meta := make([]interface{}, 11)
	meta[9] = owner
	meta[10] = []interface{}{"Owner Name", 1, nil, "Owner"}
	data := []interface{}{nil, nil, "", meta}

	got := parseContributors(data)
	if got["owner-id"] != "Owner Name" {
		t.Fatalf("expected owner fallback, got %#v", got)
	}
}

func TestParsePhotoItemsSetsPhotographer(t *testing.T) {
	alice := contributorObject("alice-id", "Alice")
	bob := contributorObject("bob-id", "Bob")
	items := []interface{}{
		photoItem("photo-1", "alice-id"),
		photoItem("photo-2", "bob-id"),
		photoItem("photo-3", "unknown-id"),
	}
	contributors := parseContributors(albumData([]interface{}{alice, bob}, items))

	photos := parsePhotoItems(items, contributors)
	if len(photos) != 3 {
		t.Fatalf("expected 3 photos, got %d", len(photos))
	}
	if photos[0].Photographer != "Alice" || photos[1].Photographer != "Bob" {
		t.Fatalf("wrong photographers: %q, %q", photos[0].Photographer, photos[1].Photographer)
	}
	if photos[2].Photographer != "" {
		t.Fatalf("unknown contributor must stay empty, got %q", photos[2].Photographer)
	}
}

func TestParsePhotoItemsSingleContributorFallback(t *testing.T) {
	alice := contributorObject("alice-id", "Alice")
	items := []interface{}{photoItem("photo-1", "")}
	contributors := parseContributors(albumData([]interface{}{alice}, items))

	photos := parsePhotoItems(items, contributors)
	if len(photos) != 1 || photos[0].Photographer != "Alice" {
		t.Fatalf("expected single contributor fallback, got %#v", photos)
	}
}

func TestParseBatchResponseKeepsPhotographer(t *testing.T) {
	alice := contributorObject("alice-id", "Alice")
	items := []interface{}{photoItem("photo-1", "alice-id")}
	payload, err := json.Marshal([]interface{}{nil, items, ""})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal([]interface{}{
		[]interface{}{"wrb.fr", "snAcKc", string(payload), nil, nil, nil, "generic"},
	})
	if err != nil {
		t.Fatal(err)
	}

	photos, _, err := parseBatchResponse(string(envelope), parseContributors(albumData([]interface{}{alice}, items)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(photos) != 1 || photos[0].Photographer != "Alice" {
		t.Fatalf("expected photographer from paginated response, got %#v", photos)
	}
}
