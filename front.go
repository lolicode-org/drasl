package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/thoas/go-funk"
	"gorm.io/gorm"
	"html/template"
	"image"
	"image/color"
	"image/png"
	"io"
	"log"
	"lukechampine.com/blake3"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
)

/*
Web front end for creating user accounts, changing passwords, skins, player names, etc.
*/

const BROWSER_TOKEN_AGE_SEC = 24 * 60 * 60

// Must be in a region of the skin that supports translucency
const SKIN_WINDOW_X_MIN = 40
const SKIN_WINDOW_X_MAX = 48
const SKIN_WINDOW_Y_MIN = 9
const SKIN_WINDOW_Y_MAX = 11

// https://echo.labstack.com/guide/templates/
// https://stackoverflow.com/questions/36617949/how-to-use-base-template-file-for-golang-html-template/69244593#69244593
type Template struct {
	Templates map[string]*template.Template
}

func NewTemplate(app *App) *Template {
	t := &Template{
		Templates: make(map[string]*template.Template),
	}

	templateDir := path.Join(app.Config.DataDirectory, "view")

	names := []string{
		"root",
		"profile",
		"registration",
		"challenge-skin",
		"admin",
	}

	for _, name := range names {
		tmpl := Unwrap(template.New("").ParseFiles(
			path.Join(templateDir, "layout.html"),
			path.Join(templateDir, name+".html"),
			path.Join(templateDir, "header.html"),
		))
		t.Templates[name] = tmpl
	}

	return t
}

func (t *Template) Render(w io.Writer, name string, data interface{}, c echo.Context) error {
	return t.Templates[name].ExecuteTemplate(w, "base", data)
}

func setSuccessMessage(c *echo.Context, message string) {
	(*c).SetCookie(&http.Cookie{
		Name:     "successMessage",
		Value:    message,
		Path:     "/",
		SameSite: http.SameSiteStrictMode,
		HttpOnly: true,
	})
}

// Set a warning message
func setWarningMessage(c *echo.Context, message string) {
	(*c).SetCookie(&http.Cookie{
		Name:     "warningMessage",
		Value:    message,
		Path:     "/",
		SameSite: http.SameSiteStrictMode,
		HttpOnly: true,
	})
}

// Set an error message cookie
func setErrorMessage(c *echo.Context, message string) {
	(*c).SetCookie(&http.Cookie{
		Name:     "errorMessage",
		Value:    message,
		Path:     "/",
		SameSite: http.SameSiteStrictMode,
		HttpOnly: true,
	})
}

func lastSuccessMessage(c *echo.Context) string {
	cookie, err := (*c).Cookie("successMessage")
	if err != nil || cookie.Value == "" {
		return ""
	}
	setSuccessMessage(c, "")
	return cookie.Value
}

func lastWarningMessage(c *echo.Context) string {
	cookie, err := (*c).Cookie("warningMessage")
	if err != nil || cookie.Value == "" {
		return ""
	}
	setWarningMessage(c, "")
	return cookie.Value
}

// Read and clear the error message cookie
func lastErrorMessage(c *echo.Context) string {
	cookie, err := (*c).Cookie("errorMessage")
	if err != nil || cookie.Value == "" {
		return ""
	}
	setErrorMessage(c, "")
	return cookie.Value
}

func getReturnURL(app *App, c *echo.Context) string {
	if (*c).FormValue("returnUrl") != "" {
		return (*c).FormValue("returnUrl")
	}
	if (*c).QueryParam("returnUrl") != "" {
		return (*c).QueryParam("username")
	}
	return app.FrontEndURL
}

// Authenticate a user using the `browserToken` cookie, and call `f` with a
// reference to the user
func withBrowserAuthentication(app *App, requireLogin bool, f func(c echo.Context, user *User) error) func(c echo.Context) error {
	return func(c echo.Context) error {
		returnURL := getReturnURL(app, &c)
		cookie, err := c.Cookie("browserToken")

		var user User
		if err != nil || cookie.Value == "" {
			if requireLogin {
				setErrorMessage(&c, "You are not logged in.")
				return c.Redirect(http.StatusSeeOther, returnURL)
			}
			return f(c, nil)
		} else {
			result := app.DB.First(&user, "browser_token = ?", cookie.Value)
			if result.Error != nil {
				if errors.Is(result.Error, gorm.ErrRecordNotFound) {
					if requireLogin {
						c.SetCookie(&http.Cookie{
							Name:     "browserToken",
							Value:    "",
							MaxAge:   -1,
							Path:     "/",
							SameSite: http.SameSiteStrictMode,
							HttpOnly: true,
						})
						setErrorMessage(&c, "You are not logged in.")
						return c.Redirect(http.StatusSeeOther, returnURL)
					}
					return f(c, nil)
				}
				return err
			}
			return f(c, &user)
		}
	}
}

// GET /
func FrontRoot(app *App) func(c echo.Context) error {
	type rootContext struct {
		App            *App
		User           *User
		URL            string
		SuccessMessage string
		WarningMessage string
		ErrorMessage   string
	}

	return withBrowserAuthentication(app, false, func(c echo.Context, user *User) error {
		return c.Render(http.StatusOK, "root", rootContext{
			App:            app,
			User:           user,
			URL:            c.Request().URL.RequestURI(),
			SuccessMessage: lastSuccessMessage(&c),
			WarningMessage: lastWarningMessage(&c),
			ErrorMessage:   lastErrorMessage(&c),
		})
	})
}

// GET /registration
func FrontRegistration(app *App) func(c echo.Context) error {
	type context struct {
		App            *App
		User           *User
		URL            string
		SuccessMessage string
		WarningMessage string
		ErrorMessage   string
		InviteCode     string
	}

	return withBrowserAuthentication(app, false, func(c echo.Context, user *User) error {
		inviteCode := c.QueryParam("invite")
		return c.Render(http.StatusOK, "registration", context{
			App:            app,
			User:           user,
			URL:            c.Request().URL.RequestURI(),
			SuccessMessage: lastSuccessMessage(&c),
			WarningMessage: lastWarningMessage(&c),
			ErrorMessage:   lastErrorMessage(&c),
			InviteCode:     inviteCode,
		})
	})
}

// GET /drasl/admin
func FrontAdmin(app *App) func(c echo.Context) error {
	type userEntry struct {
		User    User
		SkinURL *string
	}
	type adminContext struct {
		App            *App
		User           *User
		URL            string
		SuccessMessage string
		WarningMessage string
		ErrorMessage   string
		UserEntries    []userEntry
		Invites        []Invite
	}

	return withBrowserAuthentication(app, true, func(c echo.Context, user *User) error {
		returnURL := getReturnURL(app, &c)

		if !user.IsAdmin {
			setErrorMessage(&c, "You are not an admin.")
			return c.Redirect(http.StatusSeeOther, returnURL)
		}

		var users []User
		result := app.DB.Find(&users)
		if result.Error != nil {
			return result.Error
		}

		userEntries := funk.Map(users, func(u User) userEntry {
			var skinURL *string
			if u.SkinHash.Valid {
				url := SkinURL(app, u.SkinHash.String)
				skinURL = &url
			}
			return userEntry{
				User:    u,
				SkinURL: skinURL,
			}
		}).([]userEntry)

		var invites []Invite
		result = app.DB.Find(&invites)
		if result.Error != nil {
			return result.Error
		}

		return c.Render(http.StatusOK, "admin", adminContext{
			App:            app,
			User:           user,
			URL:            c.Request().URL.RequestURI(),
			SuccessMessage: lastSuccessMessage(&c),
			WarningMessage: lastWarningMessage(&c),
			ErrorMessage:   lastErrorMessage(&c),
			UserEntries:    userEntries,
			Invites:        invites,
		})
	})
}

// POST /drasl/admin/delete-user
func FrontDeleteUser(app *App) func(c echo.Context) error {
	returnURL := Unwrap(url.JoinPath(app.FrontEndURL, "drasl/admin"))

	return withBrowserAuthentication(app, true, func(c echo.Context, user *User) error {
		if !user.IsAdmin {
			setErrorMessage(&c, "You are not an admin.")
			return c.Redirect(http.StatusSeeOther, app.FrontEndURL)
		}

		username := c.FormValue("username")

		var userToDelete User
		result := app.DB.First(&userToDelete, "username = ?", username)
		if result.Error != nil {
			return result.Error
		}

		err := DeleteUser(app, &userToDelete)
		if err != nil {
			return err
		}

		return c.Redirect(http.StatusSeeOther, returnURL)
	})
}

// POST /drasl/admin/delete-invite
func FrontDeleteInvite(app *App) func(c echo.Context) error {
	returnURL := Unwrap(url.JoinPath(app.FrontEndURL, "drasl/admin"))

	return withBrowserAuthentication(app, true, func(c echo.Context, user *User) error {
		if !user.IsAdmin {
			setErrorMessage(&c, "You are not an admin.")
			return c.Redirect(http.StatusSeeOther, app.FrontEndURL)
		}

		inviteCode := c.FormValue("inviteCode")

		var invite Invite
		result := app.DB.Where("code = ?", inviteCode).Delete(&invite)
		if result.Error != nil {
			return result.Error
		}

		return c.Redirect(http.StatusSeeOther, returnURL)
	})
}

// POST /drasl/admin/update-users
func FrontUpdateUsers(app *App) func(c echo.Context) error {
	return withBrowserAuthentication(app, true, func(c echo.Context, user *User) error {
		returnURL := getReturnURL(app, &c)
		if !user.IsAdmin {
			setErrorMessage(&c, "You are not an admin.")
			return c.Redirect(http.StatusSeeOther, app.FrontEndURL)
		}

		var users []User
		result := app.DB.Find(&users)
		if result.Error != nil {
			return result.Error
		}

		tx := app.DB.Begin()

		anyUnlockedAdmins := false
		for _, user := range users {
			shouldBeAdmin := c.FormValue("admin-"+user.Username) == "on"
			shouldBeLocked := c.FormValue("locked-"+user.Username) == "on"
			if shouldBeAdmin && !shouldBeLocked {
				anyUnlockedAdmins = true
			}
			if user.IsAdmin != shouldBeAdmin || user.IsLocked != shouldBeLocked {
				user.IsAdmin = shouldBeAdmin
				err := SetIsLocked(app, &user, shouldBeLocked)
				if err != nil {
					tx.Rollback()
					return err
				}
				tx.Save(user)
			}
		}

		if !anyUnlockedAdmins {
			tx.Rollback()
			setErrorMessage(&c, "There must be at least one unlocked admin account.")
			return c.Redirect(http.StatusSeeOther, returnURL)
		}

		tx.Commit()

		setSuccessMessage(&c, "Changes saved.")
		return c.Redirect(http.StatusSeeOther, returnURL)
	})
}

// POST /drasl/admin/new-invite
func FrontNewInvite(app *App) func(c echo.Context) error {
	return withBrowserAuthentication(app, true, func(c echo.Context, user *User) error {
		returnURL := getReturnURL(app, &c)
		if !user.IsAdmin {
			setErrorMessage(&c, "You are not an admin.")
			return c.Redirect(http.StatusSeeOther, app.FrontEndURL)
		}

		_, err := app.CreateInvite()
		if err != nil {
			setErrorMessage(&c, "Error creating new invite.")
			return c.Redirect(http.StatusSeeOther, returnURL)
		}

		return c.Redirect(http.StatusSeeOther, returnURL)
	})
}

// GET /profile
func FrontProfile(app *App) func(c echo.Context) error {
	type profileContext struct {
		App            *App
		User           *User
		URL            string
		SuccessMessage string
		WarningMessage string
		ErrorMessage   string
		ProfileUser    *User
		ProfileUserID  string
		SkinURL        *string
		CapeURL        *string
	}

	return withBrowserAuthentication(app, true, func(c echo.Context, user *User) error {
		var profileUser *User
		profileUsername := c.QueryParam("user")
		if profileUsername == "" || profileUsername == user.Username {
			profileUser = user
		} else {
			if !user.IsAdmin {
				setErrorMessage(&c, "You are not an admin.")
				return c.Redirect(http.StatusSeeOther, app.FrontEndURL)
			}
			var profileUserStruct User
			result := app.DB.First(&profileUserStruct, "username = ?", profileUsername)
			profileUser = &profileUserStruct
			if result.Error != nil {
				setErrorMessage(&c, "User not found.")
				returnURL, err := url.JoinPath(app.FrontEndURL, "drasl/admin")
				if err != nil {
					return err
				}
				return c.Redirect(http.StatusSeeOther, returnURL)
			}
		}

		var skinURL *string
		if profileUser.SkinHash.Valid {
			url := SkinURL(app, profileUser.SkinHash.String)
			skinURL = &url
		}

		var capeURL *string
		if profileUser.CapeHash.Valid {
			url := CapeURL(app, profileUser.CapeHash.String)
			capeURL = &url
		}

		id, err := UUIDToID(profileUser.UUID)
		if err != nil {
			return err
		}

		warningMessage := lastWarningMessage(&c)
		if profileUser != user {
			warningMessage = strings.Join([]string{warningMessage, "Admin mode: editing profile for user \"" + profileUser.Username + "\""}, "\n")
		}

		return c.Render(http.StatusOK, "profile", profileContext{
			App:            app,
			User:           user,
			URL:            c.Request().URL.RequestURI(),
			SuccessMessage: lastSuccessMessage(&c),
			WarningMessage: warningMessage,
			ErrorMessage:   lastErrorMessage(&c),
			ProfileUser:    profileUser,
			ProfileUserID:  id,
			SkinURL:        skinURL,
			CapeURL:        capeURL,
		})
	})
}

// POST /update
func FrontUpdate(app *App) func(c echo.Context) error {
	return withBrowserAuthentication(app, true, func(c echo.Context, user *User) error {
		returnURL := getReturnURL(app, &c)

		profileUsername := c.FormValue("username")
		playerName := c.FormValue("playerName")
		fallbackPlayer := c.FormValue("fallbackPlayer")
		password := c.FormValue("password")
		preferredLanguage := c.FormValue("preferredLanguage")
		skinModel := c.FormValue("skinModel")
		skinURL := c.FormValue("skinUrl")
		deleteSkin := c.FormValue("deleteSkin") == "on"
		capeURL := c.FormValue("capeUrl")
		deleteCape := c.FormValue("deleteCape") == "on"

		var profileUser *User
		if profileUsername == "" || profileUsername == user.Username {
			profileUser = user
		} else {
			if !user.IsAdmin {
				setErrorMessage(&c, "You are not an admin.")
				return c.Redirect(http.StatusSeeOther, app.FrontEndURL)
			}
			var profileUserStruct User
			result := app.DB.First(&profileUserStruct, "username = ?", profileUsername)
			profileUser = &profileUserStruct
			if result.Error != nil {
				setErrorMessage(&c, "User not found.")
				return c.Redirect(http.StatusSeeOther, returnURL)
			}
		}

		if playerName != "" && playerName != profileUser.PlayerName {
			if err := ValidatePlayerName(app, playerName); err != nil {
				setErrorMessage(&c, fmt.Sprintf("Invalid player name: %s", err))
				return c.Redirect(http.StatusSeeOther, returnURL)
			}
			if !app.Config.AllowChangingPlayerName {
				setErrorMessage(&c, "Changing your player name is not allowed.")
				return c.Redirect(http.StatusSeeOther, returnURL)
			}
			profileUser.PlayerName = playerName
			profileUser.NameLastChangedAt = time.Now()
		}

		if fallbackPlayer != profileUser.FallbackPlayer {
			if fallbackPlayer != "" {
				if err := ValidatePlayerNameOrUUID(app, fallbackPlayer); err != nil {
					setErrorMessage(&c, fmt.Sprintf("Invalid fallback player: %s", err))
					return c.Redirect(http.StatusSeeOther, returnURL)
				}
			}
			profileUser.FallbackPlayer = fallbackPlayer
		}

		if preferredLanguage != "" {
			if !IsValidPreferredLanguage(preferredLanguage) {
				setErrorMessage(&c, "Invalid preferred language.")
				return c.Redirect(http.StatusSeeOther, returnURL)
			}
			profileUser.PreferredLanguage = preferredLanguage
		}

		if password != "" {
			if err := ValidatePassword(app, password); err != nil {
				setErrorMessage(&c, fmt.Sprintf("Invalid password: %s", err))
				return c.Redirect(http.StatusSeeOther, returnURL)
			}
			passwordSalt := make([]byte, 16)
			_, err := rand.Read(passwordSalt)
			if err != nil {
				return err
			}
			profileUser.PasswordSalt = passwordSalt

			passwordHash, err := HashPassword(password, passwordSalt)
			if err != nil {
				return err
			}
			profileUser.PasswordHash = passwordHash
		}

		if skinModel != "" {
			if !IsValidSkinModel(skinModel) {
				return c.NoContent(http.StatusBadRequest)
			}
			profileUser.SkinModel = skinModel
		}

		skinFile, skinFileErr := c.FormFile("skinFile")
		if skinFileErr == nil {
			skinHandle, err := skinFile.Open()
			if err != nil {
				return err
			}
			defer skinHandle.Close()

			validSkinHandle, err := ValidateSkin(app, skinHandle)
			if err != nil {
				setErrorMessage(&c, fmt.Sprintf("Error using that skin: %s", err))
				return c.Redirect(http.StatusSeeOther, returnURL)
			}
			err = SetSkin(app, profileUser, validSkinHandle)
			if err != nil {
				return err
			}
		} else if skinURL != "" {
			res, err := http.Get(skinURL)
			if err != nil {
				setErrorMessage(&c, "Couldn't download skin from that URL.")
				return c.Redirect(http.StatusSeeOther, returnURL)
			}
			defer res.Body.Close()

			validSkinHandle, err := ValidateSkin(app, res.Body)
			if err != nil {
				setErrorMessage(&c, fmt.Sprintf("Error using that skin: %s", err))
				return c.Redirect(http.StatusSeeOther, returnURL)
			}

			err = SetSkin(app, profileUser, validSkinHandle)
			if err != nil {
				return nil
			}
		} else if deleteSkin {
			err := SetSkin(app, profileUser, nil)
			if err != nil {
				return nil
			}
		}

		capeFile, capeFileErr := c.FormFile("capeFile")
		if capeFileErr == nil {
			capeHandle, err := capeFile.Open()
			if err != nil {
				return err
			}
			defer capeHandle.Close()

			validCapeHandle, err := ValidateCape(app, capeHandle)
			if err != nil {
				setErrorMessage(&c, fmt.Sprintf("Error using that cape: %s", err))
				return c.Redirect(http.StatusSeeOther, returnURL)
			}
			err = SetCape(app, profileUser, validCapeHandle)
			if err != nil {
				return err
			}
		} else if capeURL != "" {
			res, err := http.Get(capeURL)
			if err != nil {
				setErrorMessage(&c, "Couldn't download cape from that URL.")
				return c.Redirect(http.StatusSeeOther, returnURL)
			}
			defer res.Body.Close()

			validCapeHandle, err := ValidateCape(app, res.Body)
			if err != nil {
				setErrorMessage(&c, fmt.Sprintf("Error using that cape: %s", err))
				return c.Redirect(http.StatusSeeOther, returnURL)
			}
			err = SetCape(app, profileUser, validCapeHandle)

			if err != nil {
				return nil
			}
		} else if deleteCape {
			err := SetCape(app, profileUser, nil)
			if err != nil {
				return nil
			}
		}

		err := app.DB.Save(&profileUser).Error
		if err != nil {
			if skinHash := UnmakeNullString(&profileUser.SkinHash); skinHash != nil {
				DeleteSkinIfUnused(app, *skinHash)
			}
			if capeHash := UnmakeNullString(&profileUser.CapeHash); capeHash != nil {
				DeleteCapeIfUnused(app, *capeHash)
			}
			if IsErrorUniqueFailed(err) {
				setErrorMessage(&c, "That player name is taken.")
				return c.Redirect(http.StatusSeeOther, returnURL)
			}
			return err
		}

		setSuccessMessage(&c, "Changes saved.")
		return c.Redirect(http.StatusSeeOther, returnURL)
	})
}

// POST /logout
func FrontLogout(app *App) func(c echo.Context) error {
	return withBrowserAuthentication(app, true, func(c echo.Context, user *User) error {
		returnURL := app.FrontEndURL
		c.SetCookie(&http.Cookie{
			Name:     "browserToken",
			Value:    "",
			MaxAge:   -1,
			Path:     "/",
			SameSite: http.SameSiteStrictMode,
			HttpOnly: true,
		})
		user.BrowserToken = MakeNullString(nil)
		app.DB.Save(user)
		return c.Redirect(http.StatusSeeOther, returnURL)
	})
}

func getChallenge(app *App, username string, token string) []byte {
	// This challenge is nice because:
	// - it doesn't depend on any serverside state
	// - an attacker can't use it to verify a different username, since hash
	// incorporates the username - an attacker can't generate their own
	// challenges, since the hash includes a hash of the instance's private key
	// - an attacker can't steal the skin mid-verification and register the
	// account themselves, since the hash incorporates a token known only to
	// the verifying browser
	challengeBytes := bytes.Join([][]byte{
		[]byte(username),
		app.KeyB3Sum512,
		[]byte(token),
	}, []byte{})

	sum := blake3.Sum512(challengeBytes)
	return sum[:]
}

// GET /challenge-skin
func FrontChallengeSkin(app *App) func(c echo.Context) error {
	type challengeSkinContext struct {
		App                  *App
		User                 *User
		URL                  string
		SuccessMessage       string
		WarningMessage       string
		ErrorMessage         string
		Username             string
		RegistrationProvider string
		SkinBase64           string
		SkinFilename         string
		ChallengeToken       string
		InviteCode           string
	}

	verification_skin_path := path.Join(app.Config.DataDirectory, "assets", "verification-skin.png")
	verification_skin_file := Unwrap(os.Open(verification_skin_path))

	verification_rgba := Unwrap(png.Decode(verification_skin_file))

	verification_img, ok := verification_rgba.(*image.NRGBA)
	if !ok {
		log.Fatal("Invalid verification skin!")
	}

	return withBrowserAuthentication(app, false, func(c echo.Context, user *User) error {
		returnURL := getReturnURL(app, &c)

		username := c.QueryParam("username")
		if err := ValidateUsername(app, username); err != nil {
			setErrorMessage(&c, fmt.Sprintf("Invalid username: %s", err))
			return c.Redirect(http.StatusSeeOther, returnURL)
		}

		inviteCode := c.QueryParam("inviteCode")

		var challengeToken string
		cookie, err := c.Cookie("challengeToken")
		if err != nil || cookie.Value == "" {
			challengeToken, err = RandomHex(32)
			if err != nil {
				return err
			}
			c.SetCookie(&http.Cookie{
				Name:     "challengeToken",
				Value:    challengeToken,
				MaxAge:   BROWSER_TOKEN_AGE_SEC,
				Path:     "/",
				SameSite: http.SameSiteStrictMode,
				HttpOnly: true,
			})
		} else {
			challengeToken = cookie.Value
		}

		// challenge is a 512-bit, 64 byte checksum
		challenge := getChallenge(app, username, challengeToken)

		// Embed the challenge into a skin
		skinSize := 64
		img := image.NewNRGBA(image.Rectangle{image.Point{0, 0}, image.Point{skinSize, skinSize}})

		challengeByte := 0
		for y := 0; y < skinSize; y += 1 {
			for x := 0; x < skinSize; x += 1 {
				var col color.NRGBA
				if SKIN_WINDOW_Y_MIN <= y && y < SKIN_WINDOW_Y_MAX && SKIN_WINDOW_X_MIN <= x && x < SKIN_WINDOW_X_MAX {
					col = color.NRGBA{
						challenge[challengeByte],
						challenge[challengeByte+1],
						challenge[challengeByte+2],
						challenge[challengeByte+3],
					}
					challengeByte += 4
				} else {
					col = verification_img.At(x, y).(color.NRGBA)
				}
				img.SetNRGBA(x, y, col)
			}
		}

		var imgBuffer bytes.Buffer
		err = png.Encode(&imgBuffer, img)
		if err != nil {
			return err
		}

		skinBase64 := base64.StdEncoding.EncodeToString(imgBuffer.Bytes())
		return c.Render(http.StatusOK, "challenge-skin", challengeSkinContext{
			App:            app,
			User:           user,
			URL:            c.Request().URL.RequestURI(),
			SuccessMessage: lastSuccessMessage(&c),
			WarningMessage: lastWarningMessage(&c),
			ErrorMessage:   lastErrorMessage(&c),
			Username:       username,
			SkinBase64:     skinBase64,
			SkinFilename:   username + "-challenge.png",
			ChallengeToken: challengeToken,
			InviteCode:     inviteCode,
		})
	})
}

// type registrationUsernameToIDResponse struct {
// 	Name string `json:"name"`
// 	ID   string `json:"id"`
// }

type proxiedAccountDetails struct {
	UUID string
}

func validateChallenge(app *App, username string, challengeToken string) (*proxiedAccountDetails, error) {
	base, err := url.Parse(app.Config.RegistrationExistingPlayer.AccountURL)
	if err != nil {
		return nil, err
	}
	base.Path += "/users/profiles/minecraft/" + username

	res, err := http.Get(base.String())
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		// TODO log
		return nil, errors.New("registration server returned error")
	}

	var idRes playerNameToUUIDResponse
	err = json.NewDecoder(res.Body).Decode(&idRes)
	if err != nil {
		return nil, err
	}

	base, err = url.Parse(app.Config.RegistrationExistingPlayer.SessionURL)
	if err != nil {
		return nil, err
	}
	base.Path += "/session/minecraft/profile/" + idRes.ID

	res, err = http.Get(base.String())
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		// TODO log
		return nil, errors.New("registration server returned error")
	}

	var profileRes SessionProfileResponse
	err = json.NewDecoder(res.Body).Decode(&profileRes)
	if err != nil {
		return nil, err
	}
	id := profileRes.ID
	accountUUID, err := IDToUUID(id)
	if err != nil {
		return nil, err
	}

	details := proxiedAccountDetails{
		UUID: accountUUID,
	}
	if !app.Config.RegistrationExistingPlayer.RequireSkinVerification {
		return &details, nil
	}

	for _, property := range profileRes.Properties {
		if property.Name == "textures" {
			textureJSON, err := base64.StdEncoding.DecodeString(property.Value)
			if err != nil {
				return nil, err
			}

			var texture texturesValue
			err = json.Unmarshal(textureJSON, &texture)
			if err != nil {
				return nil, err
			}

			if texture.Textures.Skin == nil {
				return nil, errors.New("player does not have a skin")
			}
			res, err = http.Get(texture.Textures.Skin.URL)
			if err != nil {
				return nil, err
			}
			defer res.Body.Close()

			rgba_img, err := png.Decode(res.Body)
			if err != nil {
				return nil, err
			}
			img, ok := rgba_img.(*image.NRGBA)
			if !ok {
				return nil, errors.New("invalid image")
			}

			challenge := make([]byte, 64)
			challengeByte := 0
			for y := SKIN_WINDOW_Y_MIN; y < SKIN_WINDOW_Y_MAX; y += 1 {
				for x := SKIN_WINDOW_X_MIN; x < SKIN_WINDOW_X_MAX; x += 1 {
					c := img.NRGBAAt(x, y)
					challenge[challengeByte] = c.R
					challenge[challengeByte+1] = c.G
					challenge[challengeByte+2] = c.B
					challenge[challengeByte+3] = c.A

					challengeByte += 4
				}
			}

			correctChallenge := getChallenge(app, username, challengeToken)

			if !bytes.Equal(challenge, correctChallenge) {
				return nil, errors.New("skin does not match")
			}

			if err != nil {
				return nil, err
			}

			return &details, nil
		}
	}

	return nil, errors.New("registration server didn't return textures")
}

// POST /register
func FrontRegister(app *App) func(c echo.Context) error {
	returnURL := Unwrap(url.JoinPath(app.FrontEndURL, "drasl/profile"))
	return func(c echo.Context) error {
		username := c.FormValue("username")
		password := c.FormValue("password")
		chosenUUID := c.FormValue("uuid")
		existingPlayer := c.FormValue("existingPlayer") == "on"
		challengeToken := c.FormValue("challengeToken")
		inviteCode := c.FormValue("inviteCode")

		failureURL := getReturnURL(app, &c)
		noInviteFailureURL, err := StripQueryParam(failureURL, "invite")
		if err != nil {
			return err
		}

		if err := ValidateUsername(app, username); err != nil {
			setErrorMessage(&c, fmt.Sprintf("Invalid username: %s", err))
			return c.Redirect(http.StatusSeeOther, failureURL)
		}
		if err := ValidatePassword(app, password); err != nil {
			setErrorMessage(&c, fmt.Sprintf("Invalid password: %s", err))
			return c.Redirect(http.StatusSeeOther, failureURL)
		}

		var accountUUID string
		var invite Invite
		inviteUsed := false
		if existingPlayer {
			// Registration from an existing account on another server
			if !app.Config.RegistrationExistingPlayer.Allow {
				setErrorMessage(&c, "Registration from an existing account is not allowed.")
				return c.Redirect(http.StatusSeeOther, failureURL)
			}

			if app.Config.RegistrationExistingPlayer.RequireInvite {
				result := app.DB.First(&invite, "code = ?", inviteCode)
				if result.Error != nil {
					if errors.Is(result.Error, gorm.ErrRecordNotFound) {
						setErrorMessage(&c, "Invite not found!")
						return c.Redirect(http.StatusSeeOther, noInviteFailureURL)
					}
					return result.Error
				}
				inviteUsed = true
			}

			// Verify skin challenge
			details, err := validateChallenge(app, username, challengeToken)
			if err != nil {
				var message string
				if app.Config.RegistrationExistingPlayer.RequireSkinVerification {
					message = fmt.Sprintf("Couldn't verify your skin, maybe try again: %s", err)
				} else {
					message = fmt.Sprintf("Couldn't find your account, maybe try again: %s", err)
				}
				setErrorMessage(&c, message)
				return c.Redirect(http.StatusSeeOther, failureURL)
			}
			accountUUID = details.UUID
		} else {
			// New player registration
			if !app.Config.RegistrationNewPlayer.Allow {
				setErrorMessage(&c, "Registration without some existing account is not allowed.")
				return c.Redirect(http.StatusSeeOther, failureURL)
			}

			if app.Config.RegistrationNewPlayer.RequireInvite {
				result := app.DB.First(&invite, "code = ?", inviteCode)
				if result.Error != nil {
					if errors.Is(result.Error, gorm.ErrRecordNotFound) {
						setErrorMessage(&c, "Invite not found!")
						return c.Redirect(http.StatusSeeOther, noInviteFailureURL)
					}
					return result.Error
				}
				inviteUsed = true
			}

			if chosenUUID == "" {
				accountUUID = uuid.New().String()
			} else {
				if !app.Config.RegistrationNewPlayer.AllowChoosingUUID {
					setErrorMessage(&c, "Choosing a UUID is not allowed.")
					return c.Redirect(http.StatusSeeOther, failureURL)
				}
				chosenUUIDStruct, err := uuid.Parse(chosenUUID)
				if err != nil {
					message := fmt.Sprintf("Invalid UUID: %s", err)
					setErrorMessage(&c, message)
					return c.Redirect(http.StatusSeeOther, failureURL)
				}
				accountUUID = chosenUUIDStruct.String()
			}
		}

		passwordSalt := make([]byte, 16)
		_, err = rand.Read(passwordSalt)
		if err != nil {
			return err
		}

		passwordHash, err := HashPassword(password, passwordSalt)
		if err != nil {
			return err
		}

		browserToken, err := RandomHex(32)
		if err != nil {
			return err
		}

		var count int64
		result := app.DB.Model(&User{}).Count(&count)
		if result.Error != nil {
			return err
		}

		user := User{
			IsAdmin:           count == 0,
			UUID:              accountUUID,
			Username:          username,
			PasswordSalt:      passwordSalt,
			PasswordHash:      passwordHash,
			TokenPairs:        []TokenPair{},
			PlayerName:        username,
			FallbackPlayer:    accountUUID,
			PreferredLanguage: app.Config.DefaultPreferredLanguage,
			SkinModel:         SkinModelClassic,
			BrowserToken:      MakeNullString(&browserToken),
			CreatedAt:         time.Now(),
			NameLastChangedAt: time.Now(),
		}

		tx := app.DB.Begin()
		result = tx.Create(&user)
		if result.Error != nil {
			if IsErrorUniqueFailedField(result.Error, "users.username") ||
				IsErrorUniqueFailedField(result.Error, "users.player_name") {
				setErrorMessage(&c, "That username is taken.")
				tx.Rollback()
				return c.Redirect(http.StatusSeeOther, failureURL)
			} else if IsErrorUniqueFailedField(result.Error, "users.uuid") {
				setErrorMessage(&c, "That UUID is taken.")
				tx.Rollback()
				return c.Redirect(http.StatusSeeOther, failureURL)
			}
			return result.Error
		}

		if inviteUsed {
			result = tx.Delete(&invite)
			if result.Error != nil {
				tx.Rollback()
				return result.Error
			}
		}

		result = tx.Commit()
		if result.Error != nil {
			return result.Error
		}

		c.SetCookie(&http.Cookie{
			Name:     "browserToken",
			Value:    browserToken,
			MaxAge:   BROWSER_TOKEN_AGE_SEC,
			Path:     "/",
			SameSite: http.SameSiteStrictMode,
			HttpOnly: true,
		})

		return c.Redirect(http.StatusSeeOther, returnURL)
	}
}

// POST /login
func FrontLogin(app *App) func(c echo.Context) error {
	returnURL := app.FrontEndURL + "/drasl/profile"
	return func(c echo.Context) error {
		failureURL := getReturnURL(app, &c)

		username := c.FormValue("username")
		password := c.FormValue("password")

		if AnonymousLoginEligible(app, username) {
			setErrorMessage(&c, "Anonymous accounts cannot access the web interface.")
			return c.Redirect(http.StatusSeeOther, failureURL)
		}

		var user User
		result := app.DB.First(&user, "username = ?", username)
		if result.Error != nil {
			if errors.Is(result.Error, gorm.ErrRecordNotFound) {
				setErrorMessage(&c, "User not found!")
				return c.Redirect(http.StatusSeeOther, failureURL)
			}
			return result.Error
		}

		if user.IsLocked {
			setErrorMessage(&c, "Account is locked.")
			return c.Redirect(http.StatusSeeOther, failureURL)
		}

		passwordHash, err := HashPassword(password, user.PasswordSalt)
		if err != nil {
			return err
		}

		if !bytes.Equal(passwordHash, user.PasswordHash) {
			setErrorMessage(&c, "Incorrect password!")
			return c.Redirect(http.StatusSeeOther, failureURL)
		}

		browserToken, err := RandomHex(32)
		if err != nil {
			return err
		}

		c.SetCookie(&http.Cookie{
			Name:     "browserToken",
			Value:    browserToken,
			MaxAge:   BROWSER_TOKEN_AGE_SEC,
			Path:     "/",
			SameSite: http.SameSiteStrictMode,
			HttpOnly: true,
		})

		user.BrowserToken = MakeNullString(&browserToken)
		app.DB.Save(&user)

		return c.Redirect(http.StatusSeeOther, returnURL)
	}
}

// POST /delete-account
func FrontDeleteAccount(app *App) func(c echo.Context) error {
	return withBrowserAuthentication(app, true, func(c echo.Context, user *User) error {
		returnURL := app.FrontEndURL

		var targetUser *User
		targetUsername := c.FormValue("username")
		if targetUsername == "" || targetUsername == user.Username {
			targetUser = user
		} else {
			if !user.IsAdmin {
				setErrorMessage(&c, "You are not an admin.")
				return c.Redirect(http.StatusSeeOther, app.FrontEndURL)
			}
			var err error
			returnURL, err = url.JoinPath(app.FrontEndURL, "drasl/admin")
			if err != nil {
				return err
			}
			var targetUserStruct User
			result := app.DB.First(&targetUserStruct, "username = ?", targetUsername)
			targetUser = &targetUserStruct
			if result.Error != nil {
				setErrorMessage(&c, "User not found.")
				return c.Redirect(http.StatusSeeOther, returnURL)
			}
		}

		DeleteUser(app, targetUser)

		if targetUser == user {
			c.SetCookie(&http.Cookie{
				Name:     "browserToken",
				Value:    "",
				MaxAge:   -1,
				Path:     "/",
				SameSite: http.SameSiteStrictMode,
				HttpOnly: true,
			})
		}
		setSuccessMessage(&c, "Account deleted")

		return c.Redirect(http.StatusSeeOther, returnURL)
	})
}
