package cwsource

import "testing"

func TestValidateInsightsQuery_ValidQueries(t *testing.T) {
	valid := []string{
		`filter @message like /error/`,
		`filter @message like /(?i)timeout/ | stats count(*) by bin(5m)`,
		`fields @timestamp, @message | sort @timestamp desc | limit 20`,
		`stats count(*) by service`,
		`filter level = "ERROR" | stats count(*) by bin(1h)`,
	}
	for _, q := range valid {
		if err := validateInsightsQuery(q); err != nil {
			t.Errorf("expected valid query %q, got error: %v", q, err)
		}
	}
}

func TestValidateInsightsQuery_Empty(t *testing.T) {
	if err := validateInsightsQuery(""); err == nil {
		t.Error("expected error for empty query")
	}
	if err := validateInsightsQuery("   "); err == nil {
		t.Error("expected error for whitespace-only query")
	}
}

func TestValidateInsightsQuery_DangerousRegex(t *testing.T) {
	dangerous := []string{
		`filter @message like /.*/`,
		`filter @message like /.+/`,
		`parse @message /.*/ as everything`,
	}
	for _, q := range dangerous {
		if err := validateInsightsQuery(q); err == nil {
			t.Errorf("expected error for dangerous query %q", q)
		}
	}
}

func TestValidateInsightsQuery_BlockedKeywords(t *testing.T) {
	blocked := []string{
		`delete from logs`,
		`DROP table`,
		`INSERT something`,
		`update records`,
		`CREATE table`,
		`ALTER something`,
	}
	for _, q := range blocked {
		if err := validateInsightsQuery(q); err == nil {
			t.Errorf("expected error for blocked query %q", q)
		}
	}
}

func TestValidateInsightsQuery_SafeKeywordsInContext(t *testing.T) {
	// These contain blocked words as substrings but should be allowed
	safe := []string{
		`filter @message like /updated user profile/`,
		`filter @message like /created new order/`,
		`filter @message like /deleted old cache/`,
	}
	for _, q := range safe {
		if err := validateInsightsQuery(q); err != nil {
			t.Errorf("expected valid query %q, got error: %v", q, err)
		}
	}
}
