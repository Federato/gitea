// Copyright 2014 The Gogs Authors. All rights reserved.
// Copyright 2019 The Gitea Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package user

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "image/jpeg" // Needed for jpeg support

	"code.gitea.io/gitea/models/auth"
	"code.gitea.io/gitea/models/db"
	"code.gitea.io/gitea/modules/auth/openid"
	"code.gitea.io/gitea/modules/base"
	"code.gitea.io/gitea/modules/git"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/modules/structs"
	"code.gitea.io/gitea/modules/timeutil"
	"code.gitea.io/gitea/modules/util"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/pbkdf2"
	"golang.org/x/crypto/scrypt"
	"xorm.io/builder"
)

// UserType defines the user type
type UserType int //revive:disable-line:exported

const (
	// UserTypeIndividual defines an individual user
	UserTypeIndividual UserType = iota // Historic reason to make it starts at 0.

	// UserTypeOrganization defines an organization
	UserTypeOrganization
)

const (
	algoBcrypt = "bcrypt"
	algoScrypt = "scrypt"
	algoArgon2 = "argon2"
	algoPbkdf2 = "pbkdf2"
)

// AvailableHashAlgorithms represents the available password hashing algorithms
var AvailableHashAlgorithms = []string{
	algoPbkdf2,
	algoArgon2,
	algoScrypt,
	algoBcrypt,
}

const (
	// EmailNotificationsEnabled indicates that the user would like to receive all email notifications
	EmailNotificationsEnabled = "enabled"
	// EmailNotificationsOnMention indicates that the user would like to be notified via email when mentioned.
	EmailNotificationsOnMention = "onmention"
	// EmailNotificationsDisabled indicates that the user would not like to be notified via email.
	EmailNotificationsDisabled = "disabled"
)

// User represents the object of individual and member of organization.
type User struct {
	ID        int64  `xorm:"pk autoincr"`
	LowerName string `xorm:"UNIQUE NOT NULL"`
	Name      string `xorm:"UNIQUE NOT NULL"`
	FullName  string
	// Email is the primary email address (to be used for communication)
	Email                        string `xorm:"NOT NULL"`
	KeepEmailPrivate             bool
	EmailNotificationsPreference string `xorm:"VARCHAR(20) NOT NULL DEFAULT 'enabled'"`
	Passwd                       string `xorm:"NOT NULL"`
	PasswdHashAlgo               string `xorm:"NOT NULL DEFAULT 'argon2'"`

	// MustChangePassword is an attribute that determines if a user
	// is to change his/her password after registration.
	MustChangePassword bool `xorm:"NOT NULL DEFAULT false"`

	LoginType   auth.Type
	LoginSource int64 `xorm:"NOT NULL DEFAULT 0"`
	LoginName   string
	Type        UserType
	Location    string
	Website     string
	Rands       string `xorm:"VARCHAR(32)"`
	Salt        string `xorm:"VARCHAR(32)"`
	Language    string `xorm:"VARCHAR(5)"`
	Description string

	CreatedUnix   timeutil.TimeStamp `xorm:"INDEX created"`
	UpdatedUnix   timeutil.TimeStamp `xorm:"INDEX updated"`
	LastLoginUnix timeutil.TimeStamp `xorm:"INDEX"`

	// Remember visibility choice for convenience, true for private
	LastRepoVisibility bool
	// Maximum repository creation limit, -1 means use global default
	MaxRepoCreation int `xorm:"NOT NULL DEFAULT -1"`

	// IsActive true: primary email is activated, user can access Web UI and Git SSH.
	// false: an inactive user can only log in Web UI for account operations (ex: activate the account by email), no other access.
	IsActive bool `xorm:"INDEX"`
	// the user is a Gitea admin, who can access all repositories and the admin pages.
	IsAdmin bool
	// true: the user is only allowed to see organizations/repositories that they has explicit rights to.
	// (ex: in private Gitea instances user won't be allowed to see even organizations/repositories that are set as public)
	IsRestricted bool `xorm:"NOT NULL DEFAULT false"`

	AllowGitHook            bool
	AllowImportLocal        bool // Allow migrate repository by local path
	AllowCreateOrganization bool `xorm:"DEFAULT true"`

	// true: the user is not allowed to log in Web UI. Git/SSH access could still be allowed (please refer to Git/SSH access related code/documents)
	ProhibitLogin bool `xorm:"NOT NULL DEFAULT false"`

	// Avatar
	Avatar          string `xorm:"VARCHAR(2048) NOT NULL"`
	AvatarEmail     string `xorm:"NOT NULL"`
	UseCustomAvatar bool

	// Counters
	NumFollowers int
	NumFollowing int `xorm:"NOT NULL DEFAULT 0"`
	NumStars     int
	NumRepos     int

	// For organization
	NumTeams                  int
	NumMembers                int
	Visibility                structs.VisibleType `xorm:"NOT NULL DEFAULT 0"`
	RepoAdminChangeTeamAccess bool                `xorm:"NOT NULL DEFAULT false"`

	// Preferences
	DiffViewStyle       string `xorm:"NOT NULL DEFAULT ''"`
	Theme               string `xorm:"NOT NULL DEFAULT ''"`
	KeepActivityPrivate bool   `xorm:"NOT NULL DEFAULT false"`
}

func init() {
	db.RegisterModel(new(User))
}

// SearchOrganizationsOptions options to filter organizations
type SearchOrganizationsOptions struct {
	db.ListOptions
	All bool
}

// ColorFormat writes a colored string to identify this struct
func (u *User) ColorFormat(s fmt.State) {
	if u == nil {
		log.ColorFprintf(s, "%d:%s",
			log.NewColoredIDValue(0),
			log.NewColoredValue("<nil>"))
		return
	}
	log.ColorFprintf(s, "%d:%s",
		log.NewColoredIDValue(u.ID),
		log.NewColoredValue(u.Name))
}

// BeforeUpdate is invoked from XORM before updating this object.
func (u *User) BeforeUpdate() {
	if u.MaxRepoCreation < -1 {
		u.MaxRepoCreation = -1
	}

	// Organization does not need email
	u.Email = strings.ToLower(u.Email)
	if !u.IsOrganization() {
		if len(u.AvatarEmail) == 0 {
			u.AvatarEmail = u.Email
		}
	}

	u.LowerName = strings.ToLower(u.Name)
	u.Location = base.TruncateString(u.Location, 255)
	u.Website = base.TruncateString(u.Website, 255)
	u.Description = base.TruncateString(u.Description, 255)
}

// AfterLoad is invoked from XORM after filling all the fields of this object.
func (u *User) AfterLoad() {
	if u.Theme == "" {
		u.Theme = setting.UI.DefaultTheme
	}
}

// SetLastLogin set time to last login
func (u *User) SetLastLogin() {
	u.LastLoginUnix = timeutil.TimeStampNow()
}

// UpdateUserDiffViewStyle updates the users diff view style
func UpdateUserDiffViewStyle(u *User, style string) error {
	u.DiffViewStyle = style
	return UpdateUserCols(db.DefaultContext, u, "diff_view_style")
}

// UpdateUserTheme updates a users' theme irrespective of the site wide theme
func UpdateUserTheme(u *User, themeName string) error {
	u.Theme = themeName
	return UpdateUserCols(db.DefaultContext, u, "theme")
}

// GetEmail returns an noreply email, if the user has set to keep his
// email address private, otherwise the primary email address.
func (u *User) GetEmail() string {
	if u.KeepEmailPrivate {
		return fmt.Sprintf("%s@%s", u.LowerName, setting.Service.NoReplyAddress)
	}
	return u.Email
}

// GetAllUsers returns a slice of all individual users found in DB.
func GetAllUsers() ([]*User, error) {
	users := make([]*User, 0)
	return users, db.GetEngine(db.DefaultContext).OrderBy("id").Where("type = ?", UserTypeIndividual).Find(&users)
}

// IsLocal returns true if user login type is LoginPlain.
func (u *User) IsLocal() bool {
	return u.LoginType <= auth.Plain
}

// IsOAuth2 returns true if user login type is LoginOAuth2.
func (u *User) IsOAuth2() bool {
	return u.LoginType == auth.OAuth2
}

// MaxCreationLimit returns the number of repositories a user is allowed to create
func (u *User) MaxCreationLimit() int {
	if u.MaxRepoCreation <= -1 {
		return setting.Repository.MaxCreationLimit
	}
	return u.MaxRepoCreation
}

// CanCreateRepo returns if user login can create a repository
// NOTE: functions calling this assume a failure due to repository count limit; if new checks are added, those functions should be revised
func (u *User) CanCreateRepo() bool {
	if u.IsAdmin {
		return true
	}
	if u.MaxRepoCreation <= -1 {
		if setting.Repository.MaxCreationLimit <= -1 {
			return true
		}
		return u.NumRepos < setting.Repository.MaxCreationLimit
	}
	return u.NumRepos < u.MaxRepoCreation
}

// CanCreateOrganization returns true if user can create organisation.
func (u *User) CanCreateOrganization() bool {
	return u.IsAdmin || (u.AllowCreateOrganization && !setting.Admin.DisableRegularOrgCreation)
}

// CanEditGitHook returns true if user can edit Git hooks.
func (u *User) CanEditGitHook() bool {
	return !setting.DisableGitHooks && (u.IsAdmin || u.AllowGitHook)
}

// CanImportLocal returns true if user can migrate repository by local path.
func (u *User) CanImportLocal() bool {
	if !setting.ImportLocalPaths || u == nil {
		return false
	}
	return u.IsAdmin || u.AllowImportLocal
}

// DashboardLink returns the user dashboard page link.
func (u *User) DashboardLink() string {
	if u.IsOrganization() {
		return u.OrganisationLink() + "/dashboard"
	}
	return setting.AppSubURL + "/"
}

// HomeLink returns the user or organization home page link.
func (u *User) HomeLink() string {
	return setting.AppSubURL + "/" + url.PathEscape(u.Name)
}

// HTMLURL returns the user or organization's full link.
func (u *User) HTMLURL() string {
	return setting.AppURL + url.PathEscape(u.Name)
}

// OrganisationLink returns the organization sub page link.
func (u *User) OrganisationLink() string {
	return setting.AppSubURL + "/org/" + url.PathEscape(u.Name)
}

// GenerateEmailActivateCode generates an activate code based on user information and given e-mail.
func (u *User) GenerateEmailActivateCode(email string) string {
	code := base.CreateTimeLimitCode(
		fmt.Sprintf("%d%s%s%s%s", u.ID, email, u.LowerName, u.Passwd, u.Rands),
		setting.Service.ActiveCodeLives, nil)

	// Add tail hex username
	code += hex.EncodeToString([]byte(u.LowerName))
	return code
}

// GetUserFollowers returns range of user's followers.
func GetUserFollowers(u *User, listOptions db.ListOptions) ([]*User, error) {
	sess := db.GetEngine(db.DefaultContext).
		Where("follow.follow_id=?", u.ID).
		Join("LEFT", "follow", "`user`.id=follow.user_id")

	if listOptions.Page != 0 {
		sess = db.SetSessionPagination(sess, &listOptions)

		users := make([]*User, 0, listOptions.PageSize)
		return users, sess.Find(&users)
	}

	users := make([]*User, 0, 8)
	return users, sess.Find(&users)
}

// GetUserFollowing returns range of user's following.
func GetUserFollowing(u *User, listOptions db.ListOptions) ([]*User, error) {
	sess := db.GetEngine(db.DefaultContext).
		Where("follow.user_id=?", u.ID).
		Join("LEFT", "follow", "`user`.id=follow.follow_id")

	if listOptions.Page != 0 {
		sess = db.SetSessionPagination(sess, &listOptions)

		users := make([]*User, 0, listOptions.PageSize)
		return users, sess.Find(&users)
	}

	users := make([]*User, 0, 8)
	return users, sess.Find(&users)
}

// NewGitSig generates and returns the signature of given user.
func (u *User) NewGitSig() *git.Signature {
	return &git.Signature{
		Name:  u.GitName(),
		Email: u.GetEmail(),
		When:  time.Now(),
	}
}

func hashPassword(passwd, salt, algo string) (string, error) {
	var tempPasswd []byte
	var saltBytes []byte

	// There are two formats for the Salt value:
	// * The new format is a (32+)-byte hex-encoded string
	// * The old format was a 10-byte binary format
	// We have to tolerate both here but Authenticate should
	// regenerate the Salt following a successful validation.
	if len(salt) == 10 {
		saltBytes = []byte(salt)
	} else {
		var err error
		saltBytes, err = hex.DecodeString(salt)
		if err != nil {
			return "", err
		}
	}

	switch algo {
	case algoBcrypt:
		tempPasswd, _ = bcrypt.GenerateFromPassword([]byte(passwd), bcrypt.DefaultCost)
		return string(tempPasswd), nil
	case algoScrypt:
		tempPasswd, _ = scrypt.Key([]byte(passwd), saltBytes, 65536, 16, 2, 50)
	case algoArgon2:
		tempPasswd = argon2.IDKey([]byte(passwd), saltBytes, 2, 65536, 8, 50)
	case algoPbkdf2:
		fallthrough
	default:
		tempPasswd = pbkdf2.Key([]byte(passwd), saltBytes, 10000, 50, sha256.New)
	}

	return fmt.Sprintf("%x", tempPasswd), nil
}

// SetPassword hashes a password using the algorithm defined in the config value of PASSWORD_HASH_ALGO
// change passwd, salt and passwd_hash_algo fields
func (u *User) SetPassword(passwd string) (err error) {
	if len(passwd) == 0 {
		u.Passwd = ""
		u.Salt = ""
		u.PasswdHashAlgo = ""
		return nil
	}

	if u.Salt, err = GetUserSalt(); err != nil {
		return err
	}
	if u.Passwd, err = hashPassword(passwd, u.Salt, setting.PasswordHashAlgo); err != nil {
		return err
	}
	u.PasswdHashAlgo = setting.PasswordHashAlgo

	return nil
}

// ValidatePassword checks if given password matches the one belongs to the user.
func (u *User) ValidatePassword(passwd string) bool {
	tempHash, err := hashPassword(passwd, u.Salt, u.PasswdHashAlgo)
	if err != nil {
		return false
	}

	if u.PasswdHashAlgo != algoBcrypt && subtle.ConstantTimeCompare([]byte(u.Passwd), []byte(tempHash)) == 1 {
		return true
	}
	if u.PasswdHashAlgo == algoBcrypt && bcrypt.CompareHashAndPassword([]byte(u.Passwd), []byte(passwd)) == nil {
		return true
	}
	return false
}

// IsPasswordSet checks if the password is set or left empty
func (u *User) IsPasswordSet() bool {
	return len(u.Passwd) != 0
}

// IsOrganization returns true if user is actually a organization.
func (u *User) IsOrganization() bool {
	return u.Type == UserTypeOrganization
}

// DisplayName returns full name if it's not empty,
// returns username otherwise.
func (u *User) DisplayName() string {
	trimmed := strings.TrimSpace(u.FullName)
	if len(trimmed) > 0 {
		return trimmed
	}
	return u.Name
}

// GetDisplayName returns full name if it's not empty and DEFAULT_SHOW_FULL_NAME is set,
// returns username otherwise.
func (u *User) GetDisplayName() string {
	if setting.UI.DefaultShowFullName {
		trimmed := strings.TrimSpace(u.FullName)
		if len(trimmed) > 0 {
			return trimmed
		}
	}
	return u.Name
}

func gitSafeName(name string) string {
	return strings.TrimSpace(strings.NewReplacer("\n", "", "<", "", ">", "").Replace(name))
}

// GitName returns a git safe name
func (u *User) GitName() string {
	gitName := gitSafeName(u.FullName)
	if len(gitName) > 0 {
		return gitName
	}
	// Although u.Name should be safe if created in our system
	// LDAP users may have bad names
	gitName = gitSafeName(u.Name)
	if len(gitName) > 0 {
		return gitName
	}
	// Totally pathological name so it's got to be:
	return fmt.Sprintf("user-%d", u.ID)
}

// ShortName ellipses username to length
func (u *User) ShortName(length int) string {
	return base.EllipsisString(u.Name, length)
}

// IsMailable checks if a user is eligible
// to receive emails.
func (u *User) IsMailable() bool {
	return u.IsActive
}

// EmailNotifications returns the User's email notification preference
func (u *User) EmailNotifications() string {
	return u.EmailNotificationsPreference
}

// SetEmailNotifications sets the user's email notification preference
func SetEmailNotifications(u *User, set string) error {
	u.EmailNotificationsPreference = set
	if err := UpdateUserCols(db.DefaultContext, u, "email_notifications_preference"); err != nil {
		log.Error("SetEmailNotifications: %v", err)
		return err
	}
	return nil
}

func isUserExist(e db.Engine, uid int64, name string) (bool, error) {
	if len(name) == 0 {
		return false, nil
	}
	return e.
		Where("id!=?", uid).
		Get(&User{LowerName: strings.ToLower(name)})
}

// IsUserExist checks if given user name exist,
// the user name should be noncased unique.
// If uid is presented, then check will rule out that one,
// it is used when update a user name in settings page.
func IsUserExist(uid int64, name string) (bool, error) {
	return isUserExist(db.GetEngine(db.DefaultContext), uid, name)
}

// Note: As of the beginning of 2022, it is recommended to use at least
// 64 bits of salt, but NIST is already recommending to use to 128 bits.
// (16 bytes = 16 * 8 = 128 bits)
const SaltByteLength = 16

// GetUserSalt returns a random user salt token.
func GetUserSalt() (string, error) {
	rBytes, err := util.RandomBytes(SaltByteLength)
	if err != nil {
		return "", err
	}
	// Returns a 32 bytes long string.
	return hex.EncodeToString(rBytes), nil
}

// NewGhostUser creates and returns a fake user for someone has deleted his/her account.
func NewGhostUser() *User {
	return &User{
		ID:        -1,
		Name:      "Ghost",
		LowerName: "ghost",
	}
}

// NewReplaceUser creates and returns a fake user for external user
func NewReplaceUser(name string) *User {
	return &User{
		ID:        -1,
		Name:      name,
		LowerName: strings.ToLower(name),
	}
}

// IsGhost check if user is fake user for a deleted account
func (u *User) IsGhost() bool {
	if u == nil {
		return false
	}
	return u.ID == -1 && u.Name == "Ghost"
}

var (
	reservedUsernames = []string{
		".",
		"..",
		".well-known",
		"admin",
		"api",
		"assets",
		"attachments",
		"avatars",
		"captcha",
		"commits",
		"debug",
		"error",
		"explore",
		"favicon.ico",
		"ghost",
		"help",
		"install",
		"issues",
		"less",
		"login",
		"manifest.json",
		"metrics",
		"milestones",
		"new",
		"notifications",
		"org",
		"plugins",
		"pulls",
		"raw",
		"repo",
		"robots.txt",
		"search",
		"serviceworker.js",
		"stars",
		"template",
		"user",
	}

	reservedUserPatterns = []string{"*.keys", "*.gpg", "*.rss", "*.atom"}
)

// IsUsableUsername returns an error when a username is reserved
func IsUsableUsername(name string) error {
	// Validate username make sure it satisfies requirement.
	if db.AlphaDashDotPattern.MatchString(name) {
		// Note: usually this error is normally caught up earlier in the UI
		return db.ErrNameCharsNotAllowed{Name: name}
	}
	return db.IsUsableName(reservedUsernames, reservedUserPatterns, name)
}

// CreateUserOverwriteOptions are an optional options who overwrite system defaults on user creation
type CreateUserOverwriteOptions struct {
	Visibility structs.VisibleType
}

// CreateUser creates record of a new user.
func CreateUser(u *User, overwriteDefault ...*CreateUserOverwriteOptions) (err error) {
	if err = IsUsableUsername(u.Name); err != nil {
		return err
	}

	// set system defaults
	u.KeepEmailPrivate = setting.Service.DefaultKeepEmailPrivate
	u.Visibility = setting.Service.DefaultUserVisibilityMode
	u.AllowCreateOrganization = setting.Service.DefaultAllowCreateOrganization && !setting.Admin.DisableRegularOrgCreation
	u.EmailNotificationsPreference = setting.Admin.DefaultEmailNotification
	u.MaxRepoCreation = -1
	u.Theme = setting.UI.DefaultTheme

	// overwrite defaults if set
	if len(overwriteDefault) != 0 && overwriteDefault[0] != nil {
		u.Visibility = overwriteDefault[0].Visibility
	}

	ctx, committer, err := db.TxContext()
	if err != nil {
		return err
	}
	defer committer.Close()

	sess := db.GetEngine(ctx)

	// validate data
	if err := validateUser(u); err != nil {
		return err
	}

	isExist, err := isUserExist(sess, 0, u.Name)
	if err != nil {
		return err
	} else if isExist {
		return ErrUserAlreadyExist{u.Name}
	}

	isExist, err = IsEmailUsed(ctx, u.Email)
	if err != nil {
		return err
	} else if isExist {
		return ErrEmailAlreadyUsed{
			Email: u.Email,
		}
	}

	// prepare for database

	u.LowerName = strings.ToLower(u.Name)
	u.AvatarEmail = u.Email
	if u.Rands, err = GetUserSalt(); err != nil {
		return err
	}
	if err = u.SetPassword(u.Passwd); err != nil {
		return err
	}

	// save changes to database

	if err = DeleteUserRedirect(ctx, u.Name); err != nil {
		return err
	}

	if err = db.Insert(ctx, u); err != nil {
		return err
	}

	// insert email address
	if err := db.Insert(ctx, &EmailAddress{
		UID:         u.ID,
		Email:       u.Email,
		LowerEmail:  strings.ToLower(u.Email),
		IsActivated: u.IsActive,
		IsPrimary:   true,
	}); err != nil {
		return err
	}

	return committer.Commit()
}

func countUsers(e db.Engine) int64 {
	count, _ := e.
		Where("type=0").
		Count(new(User))
	return count
}

// CountUsers returns number of users.
func CountUsers() int64 {
	return countUsers(db.GetEngine(db.DefaultContext))
}

// GetVerifyUser get user by verify code
func GetVerifyUser(code string) (user *User) {
	if len(code) <= base.TimeLimitCodeLength {
		return nil
	}

	// use tail hex username query user
	hexStr := code[base.TimeLimitCodeLength:]
	if b, err := hex.DecodeString(hexStr); err == nil {
		if user, err = GetUserByName(string(b)); user != nil {
			return user
		}
		log.Error("user.getVerifyUser: %v", err)
	}

	return nil
}

// VerifyUserActiveCode verifies active code when active account
func VerifyUserActiveCode(code string) (user *User) {
	minutes := setting.Service.ActiveCodeLives

	if user = GetVerifyUser(code); user != nil {
		// time limit code
		prefix := code[:base.TimeLimitCodeLength]
		data := fmt.Sprintf("%d%s%s%s%s", user.ID, user.Email, user.LowerName, user.Passwd, user.Rands)

		if base.VerifyTimeLimitCode(data, minutes, prefix) {
			return user
		}
	}
	return nil
}

// ChangeUserName changes all corresponding setting from old user name to new one.
func ChangeUserName(u *User, newUserName string) (err error) {
	oldUserName := u.Name
	if err = IsUsableUsername(newUserName); err != nil {
		return err
	}

	ctx, committer, err := db.TxContext()
	if err != nil {
		return err
	}
	defer committer.Close()
	sess := db.GetEngine(ctx)

	isExist, err := isUserExist(sess, 0, newUserName)
	if err != nil {
		return err
	} else if isExist {
		return ErrUserAlreadyExist{newUserName}
	}

	if _, err = sess.Exec("UPDATE `repository` SET owner_name=? WHERE owner_name=?", newUserName, oldUserName); err != nil {
		return fmt.Errorf("Change repo owner name: %v", err)
	}

	// Do not fail if directory does not exist
	if err = util.Rename(UserPath(oldUserName), UserPath(newUserName)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("Rename user directory: %v", err)
	}

	if err = NewUserRedirect(ctx, u.ID, oldUserName, newUserName); err != nil {
		return err
	}

	if err = committer.Commit(); err != nil {
		if err2 := util.Rename(UserPath(newUserName), UserPath(oldUserName)); err2 != nil && !os.IsNotExist(err2) {
			log.Critical("Unable to rollback directory change during failed username change from: %s to: %s. DB Error: %v. Filesystem Error: %v", oldUserName, newUserName, err, err2)
			return fmt.Errorf("failed to rollback directory change during failed username change from: %s to: %s. DB Error: %w. Filesystem Error: %v", oldUserName, newUserName, err, err2)
		}
		return err
	}

	return nil
}

// checkDupEmail checks whether there are the same email with the user
func checkDupEmail(e db.Engine, u *User) error {
	u.Email = strings.ToLower(u.Email)
	has, err := e.
		Where("id!=?", u.ID).
		And("type=?", u.Type).
		And("email=?", u.Email).
		Get(new(User))
	if err != nil {
		return err
	} else if has {
		return ErrEmailAlreadyUsed{
			Email: u.Email,
		}
	}
	return nil
}

// validateUser check if user is valid to insert / update into database
func validateUser(u *User) error {
	if !setting.Service.AllowedUserVisibilityModesSlice.IsAllowedVisibility(u.Visibility) && !u.IsOrganization() {
		return fmt.Errorf("visibility Mode not allowed: %s", u.Visibility.String())
	}

	u.Email = strings.ToLower(u.Email)
	return ValidateEmail(u.Email)
}

func updateUser(ctx context.Context, u *User, changePrimaryEmail bool) error {
	if err := validateUser(u); err != nil {
		return err
	}

	e := db.GetEngine(ctx)

	if changePrimaryEmail {
		var emailAddress EmailAddress
		has, err := e.Where("lower_email=?", strings.ToLower(u.Email)).Get(&emailAddress)
		if err != nil {
			return err
		}
		if !has {
			// 1. Update old primary email
			if _, err = e.Where("uid=? AND is_primary=?", u.ID, true).Cols("is_primary").Update(&EmailAddress{
				IsPrimary: false,
			}); err != nil {
				return err
			}

			emailAddress.Email = u.Email
			emailAddress.UID = u.ID
			emailAddress.IsActivated = true
			emailAddress.IsPrimary = true
			if _, err := e.Insert(&emailAddress); err != nil {
				return err
			}
		} else if _, err := e.ID(emailAddress).Cols("is_primary").Update(&EmailAddress{
			IsPrimary: true,
		}); err != nil {
			return err
		}
	}

	_, err := e.ID(u.ID).AllCols().Update(u)
	return err
}

// UpdateUser updates user's information.
func UpdateUser(u *User, emailChanged bool) error {
	return updateUser(db.DefaultContext, u, emailChanged)
}

// UpdateUserCols update user according special columns
func UpdateUserCols(ctx context.Context, u *User, cols ...string) error {
	return updateUserCols(db.GetEngine(ctx), u, cols...)
}

// UpdateUserColsEngine update user according special columns
func UpdateUserColsEngine(e db.Engine, u *User, cols ...string) error {
	return updateUserCols(e, u, cols...)
}

func updateUserCols(e db.Engine, u *User, cols ...string) error {
	if err := validateUser(u); err != nil {
		return err
	}

	_, err := e.ID(u.ID).Cols(cols...).Update(u)
	return err
}

// UpdateUserSetting updates user's settings.
func UpdateUserSetting(u *User) (err error) {
	ctx, committer, err := db.TxContext()
	if err != nil {
		return err
	}
	defer committer.Close()

	if !u.IsOrganization() {
		if err = checkDupEmail(db.GetEngine(ctx), u); err != nil {
			return err
		}
	}
	if err = updateUser(ctx, u, false); err != nil {
		return err
	}
	return committer.Commit()
}

// GetInactiveUsers gets all inactive users
func GetInactiveUsers(ctx context.Context, olderThan time.Duration) ([]*User, error) {
	var cond builder.Cond = builder.Eq{"is_active": false}

	if olderThan > 0 {
		cond = cond.And(builder.Lt{"created_unix": time.Now().Add(-olderThan).Unix()})
	}

	users := make([]*User, 0, 10)
	return users, db.GetEngine(ctx).
		Where(cond).
		Find(&users)
}

// UserPath returns the path absolute path of user repositories.
func UserPath(userName string) string { //revive:disable-line:exported
	return filepath.Join(setting.RepoRootPath, strings.ToLower(userName))
}

// GetUserByIDEngine returns the user object by given ID if exists.
func GetUserByIDEngine(e db.Engine, id int64) (*User, error) {
	u := new(User)
	has, err := e.ID(id).Get(u)
	if err != nil {
		return nil, err
	} else if !has {
		return nil, ErrUserNotExist{id, "", 0}
	}
	return u, nil
}

// GetUserByID returns the user object by given ID if exists.
func GetUserByID(id int64) (*User, error) {
	return GetUserByIDCtx(db.DefaultContext, id)
}

// GetUserByIDCtx returns the user object by given ID if exists.
func GetUserByIDCtx(ctx context.Context, id int64) (*User, error) {
	return GetUserByIDEngine(db.GetEngine(ctx), id)
}

// GetUserByName returns user by given name.
func GetUserByName(name string) (*User, error) {
	return GetUserByNameCtx(db.DefaultContext, name)
}

// GetUserByNameCtx returns user by given name.
func GetUserByNameCtx(ctx context.Context, name string) (*User, error) {
	if len(name) == 0 {
		return nil, ErrUserNotExist{0, name, 0}
	}
	u := &User{LowerName: strings.ToLower(name)}
	has, err := db.GetEngine(ctx).Get(u)
	if err != nil {
		return nil, err
	} else if !has {
		return nil, ErrUserNotExist{0, name, 0}
	}
	return u, nil
}

// GetUserEmailsByNames returns a list of e-mails corresponds to names of users
// that have their email notifications set to enabled or onmention.
func GetUserEmailsByNames(names []string) []string {
	return getUserEmailsByNames(db.DefaultContext, names)
}

func getUserEmailsByNames(ctx context.Context, names []string) []string {
	mails := make([]string, 0, len(names))
	for _, name := range names {
		u, err := GetUserByNameCtx(ctx, name)
		if err != nil {
			continue
		}
		if u.IsMailable() && u.EmailNotifications() != EmailNotificationsDisabled {
			mails = append(mails, u.Email)
		}
	}
	return mails
}

// GetMaileableUsersByIDs gets users from ids, but only if they can receive mails
func GetMaileableUsersByIDs(ids []int64, isMention bool) ([]*User, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ous := make([]*User, 0, len(ids))

	if isMention {
		return ous, db.GetEngine(db.DefaultContext).In("id", ids).
			Where("`type` = ?", UserTypeIndividual).
			And("`prohibit_login` = ?", false).
			And("`is_active` = ?", true).
			And("`email_notifications_preference` IN ( ?, ?)", EmailNotificationsEnabled, EmailNotificationsOnMention).
			Find(&ous)
	}

	return ous, db.GetEngine(db.DefaultContext).In("id", ids).
		Where("`type` = ?", UserTypeIndividual).
		And("`prohibit_login` = ?", false).
		And("`is_active` = ?", true).
		And("`email_notifications_preference` = ?", EmailNotificationsEnabled).
		Find(&ous)
}

// GetUserNamesByIDs returns usernames for all resolved users from a list of Ids.
func GetUserNamesByIDs(ids []int64) ([]string, error) {
	unames := make([]string, 0, len(ids))
	err := db.GetEngine(db.DefaultContext).In("id", ids).
		Table("user").
		Asc("name").
		Cols("name").
		Find(&unames)
	return unames, err
}

// GetUserIDsByNames returns a slice of ids corresponds to names.
func GetUserIDsByNames(names []string, ignoreNonExistent bool) ([]int64, error) {
	ids := make([]int64, 0, len(names))
	for _, name := range names {
		u, err := GetUserByName(name)
		if err != nil {
			if ignoreNonExistent {
				continue
			} else {
				return nil, err
			}
		}
		ids = append(ids, u.ID)
	}
	return ids, nil
}

// GetUsersBySource returns a list of Users for a login source
func GetUsersBySource(s *auth.Source) ([]*User, error) {
	var users []*User
	err := db.GetEngine(db.DefaultContext).Where("login_type = ? AND login_source = ?", s.Type, s.ID).Find(&users)
	return users, err
}

// UserCommit represents a commit with validation of user.
type UserCommit struct { //revive:disable-line:exported
	User *User
	*git.Commit
}

// ValidateCommitWithEmail check if author's e-mail of commit is corresponding to a user.
func ValidateCommitWithEmail(c *git.Commit) *User {
	if c.Author == nil {
		return nil
	}
	u, err := GetUserByEmail(c.Author.Email)
	if err != nil {
		return nil
	}
	return u
}

// ValidateCommitsWithEmails checks if authors' e-mails of commits are corresponding to users.
func ValidateCommitsWithEmails(oldCommits []*git.Commit) []*UserCommit {
	var (
		emails     = make(map[string]*User)
		newCommits = make([]*UserCommit, 0, len(oldCommits))
	)
	for _, c := range oldCommits {
		var u *User
		if c.Author != nil {
			if v, ok := emails[c.Author.Email]; !ok {
				u, _ = GetUserByEmail(c.Author.Email)
				emails[c.Author.Email] = u
			} else {
				u = v
			}
		}

		newCommits = append(newCommits, &UserCommit{
			User:   u,
			Commit: c,
		})
	}
	return newCommits
}

// GetUserByEmail returns the user object by given e-mail if exists.
func GetUserByEmail(email string) (*User, error) {
	return GetUserByEmailContext(db.DefaultContext, email)
}

// GetUserByEmailContext returns the user object by given e-mail if exists with db context
func GetUserByEmailContext(ctx context.Context, email string) (*User, error) {
	if len(email) == 0 {
		return nil, ErrUserNotExist{0, email, 0}
	}

	email = strings.ToLower(email)
	// First try to find the user by primary email
	user := &User{Email: email}
	has, err := db.GetEngine(ctx).Get(user)
	if err != nil {
		return nil, err
	}
	if has {
		return user, nil
	}

	// Otherwise, check in alternative list for activated email addresses
	emailAddress := &EmailAddress{Email: email, IsActivated: true}
	has, err = db.GetEngine(ctx).Get(emailAddress)
	if err != nil {
		return nil, err
	}
	if has {
		return GetUserByIDCtx(ctx, emailAddress.UID)
	}

	// Finally, if email address is the protected email address:
	if strings.HasSuffix(email, fmt.Sprintf("@%s", setting.Service.NoReplyAddress)) {
		username := strings.TrimSuffix(email, fmt.Sprintf("@%s", setting.Service.NoReplyAddress))
		user := &User{}
		has, err := db.GetEngine(ctx).Where("lower_name=?", username).Get(user)
		if err != nil {
			return nil, err
		}
		if has {
			return user, nil
		}
	}

	return nil, ErrUserNotExist{0, email, 0}
}

// GetUser checks if a user already exists
func GetUser(user *User) (bool, error) {
	return db.GetEngine(db.DefaultContext).Get(user)
}

// GetUserByOpenID returns the user object by given OpenID if exists.
func GetUserByOpenID(uri string) (*User, error) {
	if len(uri) == 0 {
		return nil, ErrUserNotExist{0, uri, 0}
	}

	uri, err := openid.Normalize(uri)
	if err != nil {
		return nil, err
	}

	log.Trace("Normalized OpenID URI: " + uri)

	// Otherwise, check in openid table
	oid := &UserOpenID{}
	has, err := db.GetEngine(db.DefaultContext).Where("uri=?", uri).Get(oid)
	if err != nil {
		return nil, err
	}
	if has {
		return GetUserByID(oid.UID)
	}

	return nil, ErrUserNotExist{0, uri, 0}
}

// GetAdminUser returns the first administrator
func GetAdminUser() (*User, error) {
	var admin User
	has, err := db.GetEngine(db.DefaultContext).Where("is_admin=?", true).Get(&admin)
	if err != nil {
		return nil, err
	} else if !has {
		return nil, ErrUserNotExist{}
	}

	return &admin, nil
}
