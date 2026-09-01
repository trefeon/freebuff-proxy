package tokenhealth

import (
	"strings"
	"testing"
)

// TestClassifyEmailDomain pins the email-domain classifier: exact-domain or
// any-subdomain match, case-insensitive, with the upstream curation bar
// (lookalikes/substrings are never flagged).
func TestClassifyEmailDomain(t *testing.T) {
	cases := []struct {
		name  string
		email string
		want  EmailRiskKind
	}{
		{"mailinator", "u@mailinator.com", EmailRiskDisposable},
		{"yopmail", "u@yopmail.com", EmailRiskDisposable},
		{"gmaiko", "u@gmaiko.com", EmailRiskDisposable},
		{"duojumbo", "u@duojumbo.com", EmailRiskDisposable},
		{"subdomain mailinator", "u@sub.mailinator.com", EmailRiskDisposable},
		{"deep subdomain guerrillamail", "u@a.b.guerrillamail.net", EmailRiskDisposable},
		{"upper domain", "u@MAILINATOR.COM", EmailRiskDisposable},
		{"gmisel lookalike", "u@gmisel.com", EmailRiskClean},
		{"gmail plain", "u@gmail.com", EmailRiskClean},
		{"mailinator as suffix", "u@mailinator.com.evil.com", EmailRiskClean},
		{"proton.me", "u@proton.me", EmailRiskMainstream},
		{"duck.com", "u@duck.com", EmailRiskMainstream},
		{"passmail", "u@passmail.net", EmailRiskRelay},
		{"simplelogin io", "u@simplelogin.io", EmailRiskRelay},
		{"empty", "", EmailRiskClean},
		{"no at", "not-an-email", EmailRiskClean},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyEmailDomain(tc.email); got != tc.want {
				t.Errorf("ClassifyEmailDomain(%q) = %q, want %q", tc.email, got, tc.want)
			}
		})
	}
}

// TestMaskEmail pins the report masking: local part masked, domain kept,
// never a full raw address.
func TestMaskEmail(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"john.doe@gmail.com", "j***e@gmail.com"},
		{"a@b.com", "*@b.com"},
		{"ab@b.com", "a*@b.com"},
		{"User@PROTON.ME", "U***r@proton.me"},
		{"", ""},
		{"no-at-sign", ""},
	}
	for _, tc := range cases {
		got := MaskEmail(tc.in)
		if got != tc.want {
			t.Errorf("MaskEmail(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if tc.want != "" && strings.Contains(tc.in, "@") {
			local := tc.in[:strings.LastIndex(tc.in, "@")]
			if len(local) > 2 && got == local+"@"+strings.ToLower(tc.in[strings.LastIndex(tc.in, "@")+1:]) {
				t.Errorf("MaskEmail(%q) leaked the full address: %q", tc.in, got)
			}
		}
	}
}

// TestFlagSharedMailboxes pins the shared-mailbox heuristic: same mailbox
// (domain + normalized local part, +tag aliases stripped, case-insensitive)
// flags every affected token; different domains with the same local part
// are deliberately NOT shared.
func TestFlagSharedMailboxes(t *testing.T) {
	rows := []TokenHealth{
		{Index: 0, Email: "bob@example.com"},
		{Index: 1, Email: "BOB+newsletter@EXAMPLE.com"}, // same mailbox, +tag, case
		{Index: 2, Email: "carol@example.com"},
		{Index: 3, Email: "bob@other-domain.com"}, // same local part, different mailbox
		{Index: 4, Email: ""},
	}
	FlagSharedMailboxes(rows)
	if !rows[0].Shared || !rows[1].Shared {
		t.Errorf("rows 0/1 should be flagged shared (same mailbox): %+v", rows[:2])
	}
	if rows[2].Shared || rows[3].Shared || rows[4].Shared {
		t.Errorf("rows 2/3/4 should NOT be shared: %+v", rows[2:])
	}
	if !strings.Contains(rows[0].Hint, "SHARED") {
		t.Errorf("row 0 hint should mention SHARED: %q", rows[0].Hint)
	}
}

// TestFormatHealthReport pins the report table shape.
func TestFormatHealthReport(t *testing.T) {
	rows := []TokenHealth{
		{Index: 0, State: TokenOK, Risk: EmailRiskClean, Email: "j***e@example.com", Hint: ""},
		{Index: 1, State: TokenBanned, Risk: EmailRiskDisposable, Shared: true, Email: "u@mailinator.com", Hint: "SHARED mailbox"},
	}
	out := FormatHealthReport(rows)
	for _, want := range []string{"#", "STATE", "RISK", "SHARED", "EMAIL", "HINT", "BANNED", "disposable", "yes"} {
		if !strings.Contains(out, want) {
			t.Errorf("FormatHealthReport missing %q:\n%s", want, out)
		}
	}
}
