package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestUserIdentifier(t *testing.T) {
	tests := []struct {
		name     string
		username string
		uid      string
		want     string
	}{
		{
			name:     "username takes precedence",
			username: "user@example.com",
			uid:      "12345",
			want:     "user@example.com",
		},
		{
			name:     "uid fallback when username empty",
			username: "",
			uid:      "12345",
			want:     "12345",
		},
		{
			name:     "both empty returns empty",
			username: "",
			uid:      "",
			want:     "",
		},
		{
			name:     "service account username",
			username: "system:serviceaccount:kube-system:deployment-controller",
			uid:      "some-uid",
			want:     "system:serviceaccount:kube-system:deployment-controller",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := UserIdentifier(tt.username, tt.uid)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestHashUsername(t *testing.T) {
	tests := []struct {
		name     string
		username string
	}{
		{
			name:     "service account",
			username: "system:serviceaccount:kube-system:deployment-controller",
		},
		{
			name:     "user",
			username: "user@example.com",
		},
		{
			name:     "admin",
			username: "system:admin",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hash := HashUsername(tt.username)

			// Hash should be exactly 5 characters
			assert.Len(t, hash, 5)

			// Hash should be deterministic
			assert.Equal(t, hash, HashUsername(tt.username))

			// Hash should be base36 (alphanumeric lowercase)
			for _, c := range hash {
				assert.True(t, (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z'),
					"character %c is not base36", c)
			}
		})
	}

	// Different usernames should produce different hashes (usually)
	hash1 := HashUsername("user1")
	hash2 := HashUsername("user2")
	assert.NotEqual(t, hash1, hash2)
}

func TestRecordUpdater(t *testing.T) {
	tests := []struct {
		name        string
		existing    string
		username    string
		wantHashes  []string
		wantChanged bool
	}{
		{
			name:       "first updater",
			existing:   "",
			username:   "user1",
			wantHashes: []string{HashUsername("user1")},
		},
		{
			name:       "add second updater",
			existing:   HashUsername("user1"),
			username:   "user2",
			wantHashes: []string{HashUsername("user1"), HashUsername("user2")},
		},
		{
			name:       "duplicate updater",
			existing:   HashUsername("user1"),
			username:   "user1",
			wantHashes: []string{HashUsername("user1")},
		},
		{
			name:       "max hashes exceeded",
			existing:   "hash1,hash2,hash3,hash4,hash5",
			username:   "user1",
			wantHashes: []string{"hash2", "hash3", "hash4", "hash5", HashUsername("user1")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := &unstructured.Unstructured{}
			if tt.existing != "" {
				obj.SetAnnotations(map[string]string{
					UpdatersAnnotation: tt.existing,
				})
			}

			annotations := RecordUpdater(obj, tt.username)

			result := ParseHashes(annotations[UpdatersAnnotation])
			assert.Equal(t, tt.wantHashes, result)
		})
	}
}

func TestIdentifyController(t *testing.T) {
	user1Hash := HashUsername("user1")
	user2Hash := HashUsername("user2")
	user3Hash := HashUsername("user3")

	tests := []struct {
		name             string
		childUpdaters    string
		parentCtrls      string
		username         string
		wantController   bool
		wantCanDetermine bool
	}{
		{
			name:             "single updater is controller - match",
			childUpdaters:    user1Hash,
			parentCtrls:      "",
			username:         "user1",
			wantController:   true,
			wantCanDetermine: true,
		},
		{
			name:             "single updater is controller - no match",
			childUpdaters:    user1Hash,
			parentCtrls:      "",
			username:         "user2",
			wantController:   false,
			wantCanDetermine: true,
		},
		{
			name:             "multiple updaters with parent controllers - match intersection",
			childUpdaters:    user1Hash + "," + user2Hash,
			parentCtrls:      user1Hash,
			username:         "user1",
			wantController:   true,
			wantCanDetermine: true,
		},
		{
			name:             "multiple updaters with parent controllers - not in intersection",
			childUpdaters:    user1Hash + "," + user2Hash,
			parentCtrls:      user1Hash,
			username:         "user2",
			wantController:   false,
			wantCanDetermine: true,
		},
		{
			name:             "multiple updaters no parent controllers - can't determine",
			childUpdaters:    user1Hash + "," + user2Hash,
			parentCtrls:      "",
			username:         "user1",
			wantController:   false,
			wantCanDetermine: false,
		},
		{
			name:             "no updaters - can't determine",
			childUpdaters:    "",
			parentCtrls:      user1Hash,
			username:         "user1",
			wantController:   false,
			wantCanDetermine: false,
		},
		{
			name:             "intersection with multiple parent controllers",
			childUpdaters:    user1Hash + "," + user2Hash + "," + user3Hash,
			parentCtrls:      user1Hash + "," + user2Hash,
			username:         "user2",
			wantController:   true,
			wantCanDetermine: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			child := &unstructured.Unstructured{}
			if tt.childUpdaters != "" {
				child.SetAnnotations(map[string]string{
					UpdatersAnnotation: tt.childUpdaters,
				})
			}

			parent := &unstructured.Unstructured{}
			if tt.parentCtrls != "" {
				parent.SetAnnotations(map[string]string{
					ControllersAnnotation: tt.parentCtrls,
				})
			}

			isController, canDetermine := IdentifyController(child, parent, tt.username)
			assert.Equal(t, tt.wantController, isController, "isController mismatch")
			assert.Equal(t, tt.wantCanDetermine, canDetermine, "canDetermine mismatch")
		})
	}
}

func TestParseHashes(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"", nil},
		{"abc12", []string{"abc12"}},
		{"abc12,def34", []string{"abc12", "def34"}},
		{"abc12, def34", []string{"abc12", "def34"}},
		{" abc12 , def34 ", []string{"abc12", "def34"}},
		{"abc12,,def34", []string{"abc12", "def34"}},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := ParseHashes(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestContainsHash(t *testing.T) {
	hashes := []string{"abc12", "def34", "ghi56"}

	assert.True(t, ContainsHash(hashes, "abc12"))
	assert.True(t, ContainsHash(hashes, "def34"))
	assert.True(t, ContainsHash(hashes, "ghi56"))
	assert.False(t, ContainsHash(hashes, "xyz99"))
	assert.False(t, ContainsHash(nil, "abc12"))
}

func TestIntersect(t *testing.T) {
	tests := []struct {
		name string
		a    []string
		b    []string
		want []string
	}{
		{
			name: "empty",
			a:    nil,
			b:    nil,
			want: nil,
		},
		{
			name: "no overlap",
			a:    []string{"a", "b"},
			b:    []string{"c", "d"},
			want: nil,
		},
		{
			name: "full overlap",
			a:    []string{"a", "b"},
			b:    []string{"a", "b"},
			want: []string{"a", "b"},
		},
		{
			name: "partial overlap",
			a:    []string{"a", "b", "c"},
			b:    []string{"b", "c", "d"},
			want: []string{"b", "c"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Intersect(tt.a, tt.b)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestMaxHashes(t *testing.T) {
	require.Equal(t, 5, MaxHashes, "MaxHashes should be 5")
}
