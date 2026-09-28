package relationships

import "testing"

// Pointer fields need z.Ptr schemas; a bare z.Int()/z.Bool() against a *int/*bool field
// is a zog type-cast panic, not a validation error.
func TestParseCreateRelationshipBodyReadsPointerFields(t *testing.T) {
	var in CreateRelationshipInput
	body := []byte(`{"type":2,"from_friend_suggestion":true,"confirm_stranger_request":true}`)
	if errs := ParseCreateRelationshipBody(body, &in); errs != nil {
		t.Fatalf("parse errors: %v", errs)
	}
	if in.Type == nil || *in.Type != 2 {
		t.Errorf("Type = %v, want 2", in.Type)
	}
	if in.FromFriendSuggestion == nil || !*in.FromFriendSuggestion {
		t.Errorf("FromFriendSuggestion = %v, want true", in.FromFriendSuggestion)
	}
	if in.ConfirmStrangerRequest == nil || !*in.ConfirmStrangerRequest {
		t.Errorf("ConfirmStrangerRequest = %v, want true", in.ConfirmStrangerRequest)
	}
}
