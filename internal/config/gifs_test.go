package config

import "testing"

func TestGifsConfig(t *testing.T) {
	// Every variable is set to empty first so a developer's own environment cannot leak in.
	for _, k := range []string{"GIPHY_API_KEY", "STRAFE_GIPHY_API_KEY", "HEYPSTER_API_KEY", "STRAFE_HEYPSTER_API_KEY"} {
		t.Setenv(k, "")
	}
	if g := loadGifsConfig(); g.GiphyAPIKey != "" || g.HeypsterAPIKey != "" {
		t.Fatalf("nothing set: want no keys, got %+v", g)
	}

	// The names the web container used to read still work, so an existing .env keeps its GIFs.
	t.Setenv("STRAFE_GIPHY_API_KEY", " legacy-giphy ")
	t.Setenv("STRAFE_HEYPSTER_API_KEY", "legacy-heypster")
	if g := loadGifsConfig(); g.GiphyAPIKey != "legacy-giphy" || g.HeypsterAPIKey != "legacy-heypster" {
		t.Errorf("legacy names: got %+v", g)
	}

	// The API's own names win when both are present.
	t.Setenv("GIPHY_API_KEY", "new-giphy")
	t.Setenv("HEYPSTER_API_KEY", "new-heypster")
	if g := loadGifsConfig(); g.GiphyAPIKey != "new-giphy" || g.HeypsterAPIKey != "new-heypster" {
		t.Errorf("new names should win: got %+v", g)
	}

	// A blank new name does not shadow a real legacy one.
	t.Setenv("GIPHY_API_KEY", "   ")
	if g := loadGifsConfig(); g.GiphyAPIKey != "legacy-giphy" {
		t.Errorf("blank GIPHY_API_KEY should fall back to the legacy name, got %q", g.GiphyAPIKey)
	}
}
