package bluelink

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/oauth"
	"github.com/evcc-io/evcc/util/request"
	"github.com/google/uuid"
	"golang.org/x/net/publicsuffix"
	"golang.org/x/oauth2"
)

const (
	DeviceIdURL        = "/api/v1/spa/notifications/register"
	IntegrationInfoURL = "/api/v1/user/integrationinfo"
	SilentSigninURL    = "/api/v1/user/silentsignin"
	LanguageURL        = "/api/v1/user/language"
	LoginURL           = "/api/v1/user/signin"
	TokenURL           = "/api/v1/user/oauth2/token"
)

// Config is the bluelink API configuration
type Config struct {
	URI               string
	AuthClientID      string // v2
	BrandAuthUrl      string // v2
	BasicToken        string
	CCSPServiceID     string
	CCSPApplicationID string
}

// Identity implements the Kia/Hyundai bluelink identity.
// Based on https://github.com/Hacksore/bluelinky.
type Identity struct {
	*request.Helper
	log      *util.Logger
	config   Config
	deviceID string
	oauth2.TokenSource
}

// NewIdentity creates BlueLink Identity
func NewIdentity(log *util.Logger, config Config) *Identity {
	v := &Identity{
		log:    log,
		Helper: request.NewHelper(log),
		config: config,
	}

	return v
}

func (v *Identity) getDeviceID() (string, error) {
	stamp, err := Stamps[v.config.CCSPApplicationID].Get()
	if err != nil {
		return "", err
	}

	uuid := uuid.NewString()
	data := map[string]interface{}{
		"pushRegId": "1",
		"pushType":  "GCM",
		"uuid":      uuid,
	}

	headers := map[string]string{
		"ccsp-service-id":     v.config.CCSPServiceID,
		"ccsp-application-id": v.config.CCSPApplicationID,
		"Content-type":        "application/json;charset=UTF-8",
		"User-Agent":          "okhttp/3.10.0",
		"Stamp":               stamp,
	}

	var resp struct {
		RetCode string
		ResMsg  struct {
			DeviceID string
		}
	}

	req, err := request.New(http.MethodPost, v.config.URI+DeviceIdURL, request.MarshalJSON(data), headers)
	if err == nil {
		err = v.DoJSON(req, &resp)
	}

	return resp.ResMsg.DeviceID, err
}

func (v *Identity) getCookies() (cookieClient *request.Helper, err error) {
	cookieClient = request.NewHelper(v.log)
	cookieClient.Client.Jar, err = cookiejar.New(&cookiejar.Options{
		PublicSuffixList: publicsuffix.List,
	})

	if err == nil {
		uri := fmt.Sprintf(
			"%s/api/v1/user/oauth2/authorize?response_type=code&state=test&client_id=%s&redirect_uri=%s/api/v1/user/oauth2/redirect",
			v.config.URI,
			v.config.CCSPServiceID,
			v.config.URI,
		)

		var resp *http.Response
		if resp, err = cookieClient.Get(uri); err == nil {
			resp.Body.Close()
		}
	}

	return cookieClient, err
}

func (v *Identity) setLanguage(cookieClient *request.Helper, language string) error {
	data := map[string]interface{}{
		"lang": language,
	}

	req, err := request.New(http.MethodPost, v.config.URI+LanguageURL, request.MarshalJSON(data), request.JSONEncoding)
	if err == nil {
		var resp *http.Response
		if resp, err = cookieClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}

	return err
}

func (v *Identity) brandLogin(cookieClient *request.Helper, user, password string, vin string) (string, error) {
	req, err := request.New(http.MethodGet, v.config.URI+IntegrationInfoURL, nil, request.JSONEncoding)

	var info struct {
		UserId    string `json:"userId"`
		ServiceId string `json:"serviceId"`
	}

	if err == nil {
		err = cookieClient.DoJSON(req, &info)
	}

	var action string
	var resp *http.Response

	if err == nil {
		uri := fmt.Sprintf(v.config.BrandAuthUrl, v.config.AuthClientID, v.config.URI, "en", info.ServiceId, info.UserId)

		req, err = request.New(http.MethodGet, uri, nil)
		if err == nil {
			if resp, err = cookieClient.Do(req); err == nil {
				defer resp.Body.Close()

				var doc *goquery.Document
				if doc, err = goquery.NewDocumentFromReader(resp.Body); err == nil {
					err = errors.New("form not found")

					if form := doc.Find("form"); form != nil && form.Length() == 1 {
						var ok bool
						if action, ok = form.Attr("action"); ok {
							err = nil
						}
					}
				}
			}
		}
	}

	if err == nil {
		data := url.Values{
			"username":     []string{user},
			"password":     []string{password},
			"vin":          []string{vin},
			"credentialId": []string{""},
			"rememberMe":   []string{"on"},
		}

		req, err = request.New(http.MethodPost, action, strings.NewReader(data.Encode()), request.URLEncoding)
		if err == nil {
			cookieClient.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse } // don't follow redirects
			if resp, err = cookieClient.Do(req); err == nil {
				defer resp.Body.Close()

				// need 302
				if resp.StatusCode != http.StatusFound {
					err = errors.New("missing redirect")

					if doc, err2 := goquery.NewDocumentFromReader(resp.Body); err2 == nil {
						if span := doc.Find("span[class=kc-feedback-text]"); span != nil && span.Length() == 1 {
							err = errors.New(span.Text())
						}
					}
				}
			}

			cookieClient.CheckRedirect = nil
		}
	}

	if err == nil {
		resp, err = cookieClient.Get(resp.Header.Get("Location"))
		if err == nil {
			defer resp.Body.Close()
		}
	}

	var code string
	if err == nil {
		data := map[string]string{
			"intUserId": "",
		}

		req, err = request.New(http.MethodPost, v.config.URI+SilentSigninURL, request.MarshalJSON(data), request.JSONEncoding)
		if err == nil {
			req.Header.Set("ccsp-service-id", v.config.CCSPServiceID)
			cookieClient.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse } // don't follow redirects

			var res struct {
				RedirectUrl string `json:"redirectUrl"`
			}

			if err = cookieClient.DoJSON(req, &res); err == nil {
				var uri *url.URL
				if uri, err = url.Parse(res.RedirectUrl); err == nil {
					if code = uri.Query().Get("code"); len(code) == 0 {
						err = errors.New("code not found")
					}
				}
			}
		}
	}

	return code, err
}

func (v *Identity) bluelinkLogin(cookieClient *request.Helper, user, password string, vin string) (string, error) {
	data := map[string]interface{}{
		"email":    user,
		"password": password,
		"vin": vin,
	}

	req, err := request.New(http.MethodPost, v.config.URI+LoginURL, request.MarshalJSON(data), request.JSONEncoding)
	if err != nil {
		return "", err
	}

	var res struct {
		RedirectURL string `json:"redirectUrl"`
		ErrCode     string `json:"errCode"`
		ErrMsg      string `json:"errMsg"`
	}

	var accCode string
	if err = cookieClient.DoJSON(req, &res); err == nil {
		if parsed, err := url.Parse(res.RedirectURL); err == nil {
			accCode = parsed.Query().Get("code")
		}
	} else if res.ErrCode != "" {
		err = fmt.Errorf("%w: %s (%s)", err, res.ErrMsg, res.ErrCode)
	}

	return accCode, err
}

func (v *Identity) exchangeCode(accCode string) (oauth.Token, error) {
	headers := map[string]string{
		"Authorization": "Basic " + v.config.BasicToken,
		"Content-type":  "application/x-www-form-urlencoded",
		"User-Agent":    "okhttp/3.10.0",
	}

	data := url.Values{
		"grant_type":   {"authorization_code"},
		"redirect_uri": {v.config.URI + "/api/v1/user/oauth2/redirect"},
		"code":         {accCode},
	}

	var token oauth.Token

	req, err := request.New(http.MethodPost, v.config.URI+TokenURL, strings.NewReader(data.Encode()), headers)
	if err == nil {
		err = v.DoJSON(req, &token)
	}

	return token, err
}

// RefreshToken implements oauth.TokenRefresher
func (v *Identity) RefreshToken(token *oauth2.Token) (*oauth2.Token, error) {
	headers := map[string]string{
		"Authorization": "Basic " + v.config.BasicToken,
		"Content-type":  "application/x-www-form-urlencoded",
		"User-Agent":    "okhttp/3.10.0",
	}

	data := url.Values{
		"grant_type":    {"refresh_token"},
		"redirect_uri":  {"https://www.getpostman.com/oauth2/callback"},
		"refresh_token": {token.RefreshToken},
	}

	req, err := request.New(http.MethodPost, v.config.URI+TokenURL, strings.NewReader(data.Encode()), headers)

	var res oauth.Token
	if err == nil {
		err = v.DoJSON(req, &res)
	}

	return (*oauth2.Token)(&res), err
}

func (v *Identity) Login(user, password, language string, vin string) (err error) {
	if user == "" || password == "" {
		return api.ErrMissingCredentials
	}

	v.deviceID, err = v.getDeviceID()

	var cookieClient *request.Helper
	if err == nil {
		cookieClient, err = v.getCookies()
	}

	if err == nil {
		err = v.setLanguage(cookieClient, language)
	}

	var code string
	if err == nil {
		// try new login first, then fallback
		if code, err = v.brandLogin(cookieClient, user, password, vin); err != nil {
			code, err = v.bluelinkLogin(cookieClient, user, password, vin)
		}
	}

	if err == nil {
		var token oauth.Token
		if token, err = v.exchangeCode(code); err == nil {
			v.TokenSource = oauth.RefreshTokenSource((*oauth2.Token)(&token), v)
		}
	}

	if err != nil {
		err = fmt.Errorf("login failed: %w", err)
	}

	return err
}

// Request decorates requests with authorization headers
func (v *Identity) Request(req *http.Request) error {
	stamp, err := Stamps[v.config.CCSPApplicationID].Get()
	if err != nil {
		return err
	}

	token, err := v.Token()
	if err != nil {
		return err
	}

	for k, v := range map[string]string{
		"Authorization":       "Bearer " + token.AccessToken,
		"ccsp-device-id":      v.deviceID,
		"ccsp-application-id": v.config.CCSPApplicationID,
		"offset":              "1",
		"User-Agent":          "okhttp/3.10.0",
		"Stamp":               stamp,
	} {
		req.Header.Set(k, v)
	}

	return nil
}
