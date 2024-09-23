package database

type Bot struct {
	UserID            string `json:"user_id"`
	OwnerID           string `json:"owner_id"`
	Public            bool   `json:"public"`
	Description       string `json:"description"`
	TermsOfServiceURL string `json:"terms_of_service_url"`
	PrivacyPolicyURL  string `json:"privacy_policy_url"`
}
