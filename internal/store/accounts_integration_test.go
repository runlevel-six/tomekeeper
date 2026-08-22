package store_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

func TestValidUsername(t *testing.T) {
	cases := []struct {
		in   string
		want string
		err  error
	}{
		{"jane", "jane", nil},
		{"  jane  ", "jane", nil},
		{"Jane.Doe-2", "Jane.Doe-2", nil},
		{"", "", store.ErrUsernameBlank},
		{"   ", "", store.ErrUsernameBlank},
		// Refused rather than collapsed: a name with a space is a typo or a paste,
		// and storing "jane  doe" gives somebody a name they cannot type.
		{"jane doe", "", store.ErrUsernameInvalid},
		{"jane\tdoe", "", store.ErrUsernameInvalid},
		{"jane\ndoe", "", store.ErrUsernameInvalid},
		{"jane\x00doe", "", store.ErrUsernameInvalid},
	}

	for _, c := range cases {
		got, err := store.ValidUsername(c.in)
		if !errors.Is(err, c.err) {
			t.Errorf("ValidUsername(%q) error = %v, want %v", c.in, err, c.err)
			continue
		}
		if got != c.want {
			t.Errorf("ValidUsername(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	if _, err := store.ValidUsername(string(make([]byte, store.MaxUsernameLength+1))); err == nil {
		t.Error("ValidUsername() accepted a name past the length bound")
	}
}

func TestValidRole(t *testing.T) {
	for _, role := range []string{store.RoleAdmin, store.RoleReader} {
		if got, err := store.ValidRole(role); err != nil || got != role {
			t.Errorf("ValidRole(%q) = %q, %v", role, got, err)
		}
	}
	// A hand-crafted form must not be able to invent a privilege level.
	for _, role := range []string{"", "root", "Admin", "superuser"} {
		if _, err := store.ValidRole(role); !errors.Is(err, store.ErrInvalidRole) {
			t.Errorf("ValidRole(%q) error = %v, want ErrInvalidRole", role, err)
		}
	}
}

// A new account has no password on purpose: the two ways to get one are an
// administrator setting it, or a link the reader redeems themselves. Creating an
// account with a password would mean the administrator knows it.
func TestCreateUserStartsWithNoPassword(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)
	system := s.System()

	id, err := system.CreateUser(t.Context(), "jane", store.RoleReader)
	if err != nil {
		t.Fatalf("CreateUser() = %v", err)
	}

	account, err := system.SessionUser(t.Context(), id)
	if err != nil {
		t.Fatalf("SessionUser() = %v", err)
	}
	if account.PasswordHash != "" {
		t.Error("a new account has a password hash, so somebody else chose it")
	}
	if account.IsAdmin() {
		t.Errorf("role = %q, want %q", account.Role, store.RoleReader)
	}

	if _, err := system.CreateUser(t.Context(), "jane", store.RoleReader); !errors.Is(err, store.ErrUsernameTaken) {
		t.Errorf("creating a duplicate = %v, want ErrUsernameTaken", err)
	}
}

// An archive with no administrator cannot make one through the interface, so the
// last one must not be removable or demotable.
func TestTheLastAdministratorIsProtected(t *testing.T) {
	_, s, seed := dbtest.SetupWithUser(t)
	system := s.System()

	if err := system.DeleteUser(t.Context(), seed); !errors.Is(err, store.ErrLastAdmin) {
		t.Errorf("deleting the only admin = %v, want ErrLastAdmin", err)
	}
	if err := system.SetRole(t.Context(), seed, store.RoleReader); !errors.Is(err, store.ErrLastAdmin) {
		t.Errorf("demoting the only admin = %v, want ErrLastAdmin", err)
	}

	// With a second administrator the guard must let go, or it would be a rule
	// against ever removing anybody.
	second, err := system.CreateUser(t.Context(), "second-admin", store.RoleAdmin)
	if err != nil {
		t.Fatalf("CreateUser() = %v", err)
	}
	if err := system.SetRole(t.Context(), seed, store.RoleReader); err != nil {
		t.Errorf("demoting one of two admins = %v, want it allowed", err)
	}
	// And now the second one is the last, so it is protected in turn.
	if err := system.DeleteUser(t.Context(), second); !errors.Is(err, store.ErrLastAdmin) {
		t.Errorf("deleting the now-only admin = %v, want ErrLastAdmin", err)
	}
}

// Deleting a reader removes what was theirs and nothing that is shared. This is
// the acceptance criterion's third clause, and it holds structurally through the
// foreign keys rather than through anything this method does.
func TestDeletingAReaderKeepsTheArchive(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)
	system := s.System()

	jane, err := system.CreateUser(t.Context(), "jane", store.RoleReader)
	if err != nil {
		t.Fatalf("CreateUser() = %v", err)
	}

	articleID, _, err := s.UpsertArticle(t.Context(), store.ArticleParams{
		URLCanonical: "https://example.com/shared",
		URLOriginal:  "https://example.com/shared",
	})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}
	feedID, _, err := s.UpsertFeed(t.Context(), jane, store.FeedParams{
		FeedURL: "https://example.com/feed.xml", Title: "Example",
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}
	if _, err := s.SetStarred(t.Context(), jane, articleID, true); err != nil {
		t.Fatalf("SetStarred() = %v", err)
	}

	if err := system.DeleteUser(t.Context(), jane); err != nil {
		t.Fatalf("DeleteUser() = %v", err)
	}

	// Theirs is gone.
	var feeds, states int
	if err := s.Pool().QueryRow(t.Context(),
		`SELECT (SELECT count(*) FROM feeds WHERE id = $1),
		        (SELECT count(*) FROM article_state WHERE user_id = $2)`,
		feedID, jane).Scan(&feeds, &states); err != nil {
		t.Fatalf("counting what is left: %v", err)
	}
	if feeds != 0 {
		t.Error("the deleted reader's subscription survived")
	}
	if states != 0 {
		t.Error("the deleted reader's reading state survived")
	}

	// The article did not go with them. Another reader may be holding it.
	if _, err := s.GetArticle(t.Context(), articleID); err != nil {
		t.Errorf("GetArticle() after deleting a reader = %v; the archive is not one reader's", err)
	}
}

// The link is a credential for setting a credential, so a copy of the table must
// not contain anything usable.
func TestASetupLinkIsNotStoredInTheClear(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)
	system := s.System()

	id, err := system.CreateUser(t.Context(), "jane", store.RoleReader)
	if err != nil {
		t.Fatalf("CreateUser() = %v", err)
	}
	link, err := system.IssueSetupLink(t.Context(), id)
	if err != nil {
		t.Fatalf("IssueSetupLink() = %v", err)
	}

	var stored string
	if err := s.Pool().QueryRow(t.Context(),
		`SELECT token_sha256 FROM password_setup_links WHERE user_id = $1`, id).Scan(&stored); err != nil {
		t.Fatalf("reading the stored link: %v", err)
	}
	if stored == link.Token {
		t.Fatal("the token is stored as it was issued, so the table holds a usable credential")
	}
	if len(link.Token) < 40 {
		t.Errorf("token is %d characters; it should carry 32 bytes of randomness", len(link.Token))
	}
}

func TestASetupLinkWorksOnceAndSetsThePassword(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)
	system := s.System()

	id, err := system.CreateUser(t.Context(), "jane", store.RoleReader)
	if err != nil {
		t.Fatalf("CreateUser() = %v", err)
	}
	before, err := system.SessionUser(t.Context(), id)
	if err != nil {
		t.Fatalf("SessionUser() = %v", err)
	}

	link, err := system.IssueSetupLink(t.Context(), id)
	if err != nil {
		t.Fatalf("IssueSetupLink() = %v", err)
	}

	// Looking at it does not spend it, or the page that greets somebody by name
	// would consume the link before they typed anything.
	if _, err := system.SetupLinkAccount(t.Context(), link.Token); err != nil {
		t.Fatalf("SetupLinkAccount() = %v", err)
	}

	after, err := system.RedeemSetupLink(t.Context(), link.Token, "a-hash", "an-api-key")
	if err != nil {
		t.Fatalf("RedeemSetupLink() = %v", err)
	}
	if after.PasswordHash != "a-hash" {
		t.Errorf("password hash = %q, want the one redeemed", after.PasswordHash)
	}
	// A reset is exactly when somebody else may be holding a live session.
	if after.SessionEpoch == before.SessionEpoch {
		t.Error("redeeming a link left session_epoch alone, so existing sessions survive a reset")
	}

	// Once, and once only.
	if _, err := system.RedeemSetupLink(t.Context(), link.Token, "another-hash", "another-key"); !errors.Is(err, store.ErrLinkUnusable) {
		t.Errorf("redeeming a spent link = %v, want ErrLinkUnusable", err)
	}
	if _, err := system.SetupLinkAccount(t.Context(), link.Token); !errors.Is(err, store.ErrLinkUnusable) {
		t.Errorf("looking up a spent link = %v, want ErrLinkUnusable", err)
	}
}

// Issuing a new link stops the old one working. Re-issuing usually means the
// first went astray, and a link that went astray should not still open the door.
func TestIssuingALinkSupersedesTheEarlierOne(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)
	system := s.System()

	id, err := system.CreateUser(t.Context(), "jane", store.RoleReader)
	if err != nil {
		t.Fatalf("CreateUser() = %v", err)
	}

	first, err := system.IssueSetupLink(t.Context(), id)
	if err != nil {
		t.Fatalf("IssueSetupLink() = %v", err)
	}
	second, err := system.IssueSetupLink(t.Context(), id)
	if err != nil {
		t.Fatalf("IssueSetupLink() = %v", err)
	}
	if first.Token == second.Token {
		t.Fatal("two issued links carry the same token")
	}

	if _, err := system.RedeemSetupLink(t.Context(), first.Token, "h", "k"); !errors.Is(err, store.ErrLinkUnusable) {
		t.Errorf("the superseded link = %v, want ErrLinkUnusable", err)
	}
	if _, err := system.RedeemSetupLink(t.Context(), second.Token, "h", "k"); err != nil {
		t.Errorf("the current link = %v, want it to work", err)
	}
}

func TestAnExpiredSetupLinkIsRefused(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)
	system := s.System()

	id, err := system.CreateUser(t.Context(), "jane", store.RoleReader)
	if err != nil {
		t.Fatalf("CreateUser() = %v", err)
	}
	link, err := system.IssueSetupLink(t.Context(), id)
	if err != nil {
		t.Fatalf("IssueSetupLink() = %v", err)
	}

	// Aged rather than waited for: the expiry is a week, and a test that slept
	// would be a week long.
	if _, err := s.Pool().Exec(t.Context(),
		`UPDATE password_setup_links SET expires_at = now() - interval '1 minute' WHERE user_id = $1`,
		id); err != nil {
		t.Fatalf("expiring the link: %v", err)
	}

	if _, err := system.RedeemSetupLink(t.Context(), link.Token, "h", "k"); !errors.Is(err, store.ErrLinkUnusable) {
		t.Errorf("an expired link = %v, want ErrLinkUnusable", err)
	}
	if _, err := system.SetupLinkAccount(t.Context(), link.Token); !errors.Is(err, store.ErrLinkUnusable) {
		t.Errorf("looking up an expired link = %v, want ErrLinkUnusable", err)
	}
}

// Unknown, expired and spent are one answer. The difference is of no use to
// whoever holds the link and of some use to whoever does not.
func TestAnUnknownTokenIsRefusedLikeAnyOther(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)
	system := s.System()

	for _, token := range []string{"", "not-a-token", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"} {
		if _, err := system.RedeemSetupLink(t.Context(), token, "h", "k"); !errors.Is(err, store.ErrLinkUnusable) {
			t.Errorf("RedeemSetupLink(%q) = %v, want ErrLinkUnusable", token, err)
		}
	}
}

func TestIssuingALinkForNobody(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)

	if _, err := s.System().IssueSetupLink(t.Context(), store.UserID(999999)); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("IssueSetupLink() for a missing account = %v, want pgx.ErrNoRows", err)
	}
}

// Deleting an account takes its outstanding links with it, or a link issued
// moments before a deletion would name a user id that no longer exists.
func TestDeletingAnAccountRemovesItsLinks(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)
	system := s.System()

	id, err := system.CreateUser(t.Context(), "jane", store.RoleReader)
	if err != nil {
		t.Fatalf("CreateUser() = %v", err)
	}
	link, err := system.IssueSetupLink(t.Context(), id)
	if err != nil {
		t.Fatalf("IssueSetupLink() = %v", err)
	}
	if err := system.DeleteUser(t.Context(), id); err != nil {
		t.Fatalf("DeleteUser() = %v", err)
	}

	if _, err := system.RedeemSetupLink(t.Context(), link.Token, "h", "k"); !errors.Is(err, store.ErrLinkUnusable) {
		t.Errorf("a link for a deleted account = %v, want ErrLinkUnusable", err)
	}
}

func TestListAccountsReportsWhatTheAdminNeeds(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)
	system := s.System()

	id, err := system.CreateUser(t.Context(), "jane", store.RoleReader)
	if err != nil {
		t.Fatalf("CreateUser() = %v", err)
	}
	if _, err := system.IssueSetupLink(t.Context(), id); err != nil {
		t.Fatalf("IssueSetupLink() = %v", err)
	}

	// A second account *with* a password, because the assertion below — that no
	// hash comes back — is vacuous against an account that has none. Found by
	// neutering: setting PasswordHash in the scan changed nothing, since the value
	// being copied was the empty string either way.
	withPassword, err := system.CreateUser(t.Context(), "hasapassword", store.RoleReader)
	if err != nil {
		t.Fatalf("CreateUser() = %v", err)
	}
	if err := system.SetPassword(t.Context(), withPassword, "a-stored-hash", "an-api-key"); err != nil {
		t.Fatalf("SetPassword() = %v", err)
	}

	accounts, err := system.ListAccounts(t.Context())
	if err != nil {
		t.Fatalf("ListAccounts() = %v", err)
	}

	var jane *store.AccountSummary
	for i := range accounts {
		if accounts[i].Username == "jane" {
			jane = &accounts[i]
		}
	}
	if jane == nil {
		t.Fatal("the account just created is not in the list")
	}
	if jane.HasPassword {
		t.Error("HasPassword is true for an account with no password")
	}
	if jane.PendingLink == nil {
		t.Error("PendingLink is nil while a link is outstanding")
	}

	// The hash itself must not travel to a page. Whether there is one may.
	for _, a := range accounts {
		if a.PasswordHash != "" {
			t.Errorf("ListAccounts returned a password hash for %q", a.Username)
		}
		if a.Username == "hasapassword" && !a.HasPassword {
			t.Error("HasPassword is false for an account that has one")
		}
	}
}
