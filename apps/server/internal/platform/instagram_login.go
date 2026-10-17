package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	instagramLoginAuthorizeURL = "https://www.instagram.com/oauth/authorize"
	instagramLoginTokenURL     = "https://api.instagram.com/oauth/access_token"
	instagramLoginGraphBaseURL = "https://graph.instagram.com"

	// Instagram long-lived tokens last 60 days. The token manager refreshes a
	// grant five minutes before the reported expiry, so reporting 50 days
	// leaves ten days of retry slack before the provider token actually dies.
	instagramLoginTokenMaxLifetime = 50 * 24 * time.Hour

	instagramLoginMeFields = "user_id,username,name,profile_picture_url,account_type"
)

var (
	_ AccountSelectionAdapter     = (*InstagramLoginAdapter)(nil)
	_ AuthorizationGrantDescriber = (*InstagramLoginAdapter)(nil)
)

// InstagramLoginAdapter connects an Instagram professional account through
// Business Login for Instagram, with no Facebook Page involved. Publishing,
// comments, analytics, and resume logic come from the embedded adapter, which
// talks to graph.instagram.com instead of graph.facebook.com.
type InstagramLoginAdapter struct {
	*InstagramAdapter
	authorizeURL string
	tokenURL     string
}

func NewInstagramLoginAdapter(clientID, clientSecret, redirectURI string) *InstagramLoginAdapter {
	base := NewInstagramAdapter(clientID, clientSecret, redirectURI)
	base.graphBaseURL = instagramLoginGraphBaseURL
	return &InstagramLoginAdapter{
		InstagramAdapter: base,
		authorizeURL:     instagramLoginAuthorizeURL,
		tokenURL:         instagramLoginTokenURL,
	}
}

func instagramLoginScopes() []string {
	return []string{
		"instagram_business_basic",
		"instagram_business_content_publish",
		"instagram_business_manage_comments",
		"instagram_business_manage_insights",
	}
}

func (i *InstagramLoginAdapter) AuthorizationGrantDescriptor() AuthorizationGrantDescriptor {
	return AuthorizationGrantDescriptor{
		ProjectID:     i.clientID,
		ExecutionMode: "oauth2",
		Evidence: map[string]string{
			"protocol": "oauth2", "exchange": "authorization_code",
			"connection_type": ConnectionModeInstagramLogin, "graph_host": "graph.instagram.com",
			"graph_version": i.graphVersion,
		},
	}
}

func (i *InstagramLoginAdapter) GenerateAuthURL(state string) (string, map[string]string) {
	params := url.Values{}
	params.Set(oauthParamClientID, i.clientID)
	params.Set(oauthParamRedirectURI, i.redirectURI)
	params.Set("response_type", oauthResponseType)
	params.Set("scope", strings.Join(instagramLoginScopes(), ","))
	params.Set("state", state)
	params.Set("enable_fb_login", "false")
	return i.authorizeURL + "?" + params.Encode(), nil
}

// instagramPermissionList decodes the permissions field, which Meta documents
// as a comma-separated string and has also returned as a JSON array.
type instagramPermissionList []string

func (p *instagramPermissionList) UnmarshalJSON(data []byte) error {
	var list []string
	if err := json.Unmarshal(data, &list); err != nil {
		var joined string
		if err := json.Unmarshal(data, &joined); err != nil {
			return fmt.Errorf("permissions must be a string or an array of strings")
		}
		list = strings.FieldsFunc(joined, func(r rune) bool { return r == ',' || r == ' ' })
	}
	cleaned := make([]string, 0, len(list))
	for _, permission := range list {
		if permission = strings.TrimSpace(permission); permission != "" {
			cleaned = append(cleaned, permission)
		}
	}
	*p = cleaned
	return nil
}

type instagramLoginCodeGrant struct {
	AccessToken string                  `json:"access_token"`
	Permissions instagramPermissionList `json:"permissions"`
}

// decodeInstagramLoginCodeGrant accepts both the flat response and the
// {"data":[...]} wrapper. The Instagram-scoped user_id in either shape is
// ignored because the professional account id comes from /me.
func decodeInstagramLoginCodeGrant(body []byte) (instagramLoginCodeGrant, error) {
	var response struct {
		instagramLoginCodeGrant
		Data []instagramLoginCodeGrant `json:"data"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return instagramLoginCodeGrant{}, fmt.Errorf("decoding instagram token: %w", err)
	}
	grant := response.instagramLoginCodeGrant
	if len(response.Data) > 0 {
		grant = response.Data[0]
	}
	if grant.AccessToken == "" {
		return instagramLoginCodeGrant{}, fmt.Errorf("instagram token exchange returned no access token")
	}
	return grant, nil
}

func (i *InstagramLoginAdapter) ExchangeCode(ctx context.Context, code string, _ map[string]string) (*TokenResult, error) {
	body, err := DoFormURLEncoded(ctx, "POST", i.tokenURL, map[string]string{
		oauthParamClientID:     i.clientID,
		oauthParamClientSecret: i.clientSecret,
		grantType:              oauthGrantAuthCode,
		oauthParamRedirectURI:  i.redirectURI,
		oauthParamCode:         code,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("instagram token exchange: %w", err)
	}
	grant, err := decodeInstagramLoginCodeGrant(body)
	if err != nil {
		return nil, err
	}

	params := url.Values{
		grantType:              {"ig_exchange_token"},
		oauthParamClientSecret: {i.clientSecret},
		oauthParamAccessToken:  {grant.AccessToken},
	}
	longLived, err := i.requestLongLivedToken(ctx, "access_token", params, "instagram long-lived token")
	if err != nil {
		return nil, err
	}

	profile, err := i.fetchMe(ctx, longLived.AccessToken)
	if err != nil {
		return nil, err
	}
	longLived.Extra = map[string]string{
		"user_id": profile.UserID,
		"scope":   strings.Join(grant.Permissions, ","),
	}
	return longLived, nil
}

func (i *InstagramLoginAdapter) requestLongLivedToken(ctx context.Context, path string, params url.Values, label string) (*TokenResult, error) {
	body, err := DoRequest(ctx, "GET", i.graphBaseURL+"/"+path+"?"+params.Encode(), nil, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	var response struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", label, err)
	}
	if response.AccessToken == "" {
		return nil, fmt.Errorf("%s returned no access token", label)
	}
	return &TokenResult{
		AccessToken: response.AccessToken,
		ExpiresIn:   instagramLoginExpiresIn(response.ExpiresIn),
		TokenType:   tokenTypeBearer,
	}, nil
}

func instagramLoginExpiresIn(providerSeconds int) int {
	limit := int(instagramLoginTokenMaxLifetime / time.Second)
	if providerSeconds <= 0 || providerSeconds > limit {
		return limit
	}
	return providerSeconds
}

func (i *InstagramLoginAdapter) RefreshCapability() RefreshCapability {
	return RefreshCapability{Supported: true, CredentialSource: RefreshCredentialAccessToken}
}

func (i *InstagramLoginAdapter) RefreshToken(ctx context.Context, input RefreshTokenInput) (*TokenResult, error) {
	if input.AccessToken == "" {
		return nil, fmt.Errorf("instagram refresh requires an access token")
	}
	params := url.Values{
		grantType:             {"ig_refresh_token"},
		oauthParamAccessToken: {input.AccessToken},
	}
	return i.requestLongLivedToken(ctx, "refresh_access_token", params, "instagram refresh")
}

type instagramLoginMe struct {
	UserID            string `json:"user_id"`
	Username          string `json:"username"`
	Name              string `json:"name"`
	ProfilePictureURL string `json:"profile_picture_url"`
	AccountType       string `json:"account_type"`
}

// UnmarshalJSON reads user_id verbatim because Meta returns it as a JSON
// number in some responses and as a string in others, and professional account
// ids exceed what float64 represents exactly.
func (m *instagramLoginMe) UnmarshalJSON(data []byte) error {
	type plain instagramLoginMe
	var raw struct {
		plain
		UserID json.RawMessage `json:"user_id"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*m = instagramLoginMe(raw.plain)
	id := strings.Trim(strings.TrimSpace(string(raw.UserID)), `"`)
	if id != "null" {
		m.UserID = id
	}
	return nil
}

func (i *InstagramLoginAdapter) fetchMe(ctx context.Context, accessToken string) (*instagramLoginMe, error) {
	query := url.Values{
		"fields":              {instagramLoginMeFields},
		oauthParamAccessToken: {accessToken},
	}
	body, err := DoRequest(ctx, "GET", i.graphURL("me")+"?"+query.Encode(), nil, nil)
	if err != nil {
		return nil, fmt.Errorf("instagram profile: %w", err)
	}
	// Meta documents /me both flat and wrapped in {"data":[...]}.
	var response struct {
		Data []instagramLoginMe `json:"data"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("decoding instagram profile: %w", err)
	}
	var me instagramLoginMe
	if len(response.Data) > 0 {
		me = response.Data[0]
	} else if err := json.Unmarshal(body, &me); err != nil {
		return nil, fmt.Errorf("decoding instagram profile: %w", err)
	}
	if strings.TrimSpace(me.UserID) == "" {
		return nil, fmt.Errorf("instagram profile returned no user_id")
	}
	return &me, nil
}

func instagramLoginCapabilityState() map[string]string {
	return map[string]string{"connection_type": ConnectionModeInstagramLogin}
}

func (i *InstagramLoginAdapter) GetProfile(ctx context.Context, accessToken string) (*UserProfile, error) {
	me, err := i.fetchMe(ctx, accessToken)
	if err != nil {
		return nil, err
	}
	return &UserProfile{
		ID:              me.UserID,
		Username:        firstNonEmptyString(me.Username, me.Name),
		DisplayName:     firstNonEmptyString(me.Name, me.Username),
		AvatarURL:       me.ProfilePictureURL,
		CapabilityState: instagramLoginCapabilityState(),
	}, nil
}

func (i *InstagramLoginAdapter) ListAccountSelections(ctx context.Context, token *TokenResult) ([]AccountSelectionOption, error) {
	me, err := i.fetchMe(ctx, token.AccessToken)
	if err != nil {
		return nil, err
	}
	return []AccountSelectionOption{{
		ID:          me.UserID,
		Username:    firstNonEmptyString(me.Username, me.Name),
		DisplayName: firstNonEmptyString(me.Name, me.Username),
		AvatarURL:   me.ProfilePictureURL,
		Kind:        "Instagram professional",
		Description: firstNonEmptyString(me.AccountType, "Business or Creator account"),
		Extra:       map[string]string{"account_type": strings.ToLower(me.AccountType)},
	}}, nil
}

func (i *InstagramLoginAdapter) SelectAccount(ctx context.Context, token *TokenResult, selectionID string) (*SelectedAccount, error) {
	me, err := i.fetchMe(ctx, token.AccessToken)
	if err != nil {
		return nil, err
	}
	if me.UserID != selectionID {
		return nil, fmt.Errorf("instagram account selection %s was not found", selectionID)
	}
	return &SelectedAccount{
		AccountID:        me.UserID,
		AccountUsername:  firstNonEmptyString(me.Username, me.Name),
		AccountAvatarURL: me.ProfilePictureURL,
		Token:            token,
		CapabilityState:  instagramLoginCapabilityState(),
	}, nil
}

// SearchPublishingOptions serves an empty list because the Facebook
// pages/search location lookup does not exist on graph.instagram.com.
func (i *InstagramLoginAdapter) SearchPublishingOptions(_ context.Context, _ string, input PublishingOptionsInput) (PublishingOptionsPage, error) {
	if input.Source != "instagram_locations" {
		return PublishingOptionsPage{}, fmt.Errorf("instagram publishing option source %q is not supported", input.Source)
	}
	return PublishingOptionsPage{}, nil
}

func (i *InstagramLoginAdapter) MessagingSupport() MessagingSupport {
	return MessagingSupport{
		Unavailable: "Instagram messaging is not available for accounts connected with Instagram Login. Connect through a Facebook Page to use it.",
	}
}

func (i *InstagramLoginAdapter) EngagementSupport() EngagementSupport {
	support := i.InstagramAdapter.EngagementSupport()
	support.RequiredScopes = []string{"instagram_business_manage_comments"}
	return support
}

func (i *InstagramLoginAdapter) AnalyticsSupport() AnalyticsSupport {
	support := i.InstagramAdapter.AnalyticsSupport()
	support.ContentRequiredScopes = []string{"instagram_business_manage_insights"}
	return support
}

func (i *InstagramLoginAdapter) NativePostSupport() NativePostSupport {
	return nativePostSupport("instagram_business_basic")
}
