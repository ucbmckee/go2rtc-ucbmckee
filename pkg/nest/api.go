package nest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type API struct {
	// Deprecated: requests use the shared auth state; these fields are snapshots
	// retained for source compatibility with older callers.
	Token     string
	ExpiresAt time.Time
	auth      *authState

	StreamProjectID string
	StreamDeviceID  string
	StreamExpiresAt time.Time

	// WebRTC
	StreamSessionID string

	// RTSP
	StreamToken          string
	StreamExtensionToken string
	streamMu             sync.RWMutex
	streamGeneration     uint64

	extendMu       sync.Mutex
	extendTimer    *time.Timer
	extendDone     chan struct{}
	extendFinished chan struct{}
	extendCancel   context.CancelFunc
	extendRequest  func(context.Context) error
}

type authState struct {
	mu           sync.Mutex
	clientID     string
	clientSecret string
	refreshToken string
	token        string
	expiresAt    time.Time
}

type Auth struct {
	AccessToken string
}

type DeviceInfo struct {
	Name      string
	DeviceID  string
	Protocols []string
}

const apiRequestTimeout = 10 * time.Second

type authKey struct {
	clientID     string
	clientSecret string
	refreshToken string
}

var cache = map[authKey]*authState{}
var cacheMu sync.Mutex

func NewAPI(clientID, clientSecret, refreshToken string) (*API, error) {
	key := authKey{clientID: clientID, clientSecret: clientSecret, refreshToken: refreshToken}
	cacheMu.Lock()
	state := cache[key]
	if state == nil {
		state = &authState{clientID: clientID, clientSecret: clientSecret, refreshToken: refreshToken}
		cache[key] = state
	}
	cacheMu.Unlock()

	if err := state.ensureToken(); err != nil {
		return nil, err
	}

	return newAPI(state), nil
}

func (s *authState) ensureToken() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && time.Now().Before(s.expiresAt) {
		return nil
	}
	return s.refreshLocked()
}

func (s *authState) refreshLocked() error {
	data := url.Values{
		"grant_type":    []string{"refresh_token"},
		"client_id":     []string{s.clientID},
		"client_secret": []string{s.clientSecret},
		"refresh_token": []string{s.refreshToken},
	}

	client := &http.Client{Timeout: apiRequestTimeout}
	res, err := client.PostForm("https://www.googleapis.com/oauth2/v4/token", data)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		return errors.New("nest: wrong status: " + res.Status)
	}

	var resv struct {
		AccessToken string        `json:"access_token"`
		ExpiresIn   time.Duration `json:"expires_in"`
	}
	if err = json.NewDecoder(res.Body).Decode(&resv); err != nil {
		return err
	}

	s.token = resv.AccessToken
	s.expiresAt = time.Now().Add(resv.ExpiresIn * time.Second)
	return nil
}

func newAPI(state *authState) *API {
	state.mu.Lock()
	defer state.mu.Unlock()
	return &API{Token: state.token, ExpiresAt: state.expiresAt, auth: state}
}

func (a *API) token() string {
	if a.auth == nil {
		return a.Token
	}
	a.auth.mu.Lock()
	defer a.auth.mu.Unlock()
	return a.auth.token
}

func (a *API) GetDevices(projectID string) ([]DeviceInfo, error) {
	uri := "https://smartdevicemanagement.googleapis.com/v1/enterprises/" + projectID + "/devices"
	req, err := http.NewRequest("GET", uri, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+a.token())

	client := &http.Client{Timeout: apiRequestTimeout}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		return nil, errors.New("nest: wrong status: " + res.Status)
	}

	var resv struct {
		Devices []Device
	}

	if err = json.NewDecoder(res.Body).Decode(&resv); err != nil {
		return nil, err
	}

	devices := make([]DeviceInfo, 0, len(resv.Devices))

	for _, device := range resv.Devices {
		// only RTSP and WEB_RTC available (both supported)
		if len(device.Traits.SdmDevicesTraitsCameraLiveStream.SupportedProtocols) == 0 {
			continue
		}

		i := strings.LastIndexByte(device.Name, '/')
		if i <= 0 {
			continue
		}

		name := device.Traits.SdmDevicesTraitsInfo.CustomName
		// Devices configured through the Nest app use the container/room name as opposed to the customName trait
		if name == "" && len(device.ParentRelations) > 0 {
			name = device.ParentRelations[0].DisplayName
		}

		devices = append(devices, DeviceInfo{
			Name:      name,
			DeviceID:  device.Name[i+1:],
			Protocols: device.Traits.SdmDevicesTraitsCameraLiveStream.SupportedProtocols,
		})
	}

	return devices, nil
}

func (a *API) ExchangeSDP(projectID, deviceID, offer string) (string, error) {
	var reqv struct {
		Command string `json:"command"`
		Params  struct {
			Offer string `json:"offerSdp"`
		} `json:"params"`
	}
	reqv.Command = "sdm.devices.commands.CameraLiveStream.GenerateWebRtcStream"
	reqv.Params.Offer = offer

	b, err := json.Marshal(reqv)
	if err != nil {
		return "", err
	}

	uri := "https://smartdevicemanagement.googleapis.com/v1/enterprises/" +
		projectID + "/devices/" + deviceID + ":executeCommand"

	maxRetries := 3
	retryDelay := time.Second * 30

	for attempt := 0; attempt < maxRetries; attempt++ {
		req, err := http.NewRequest("POST", uri, bytes.NewReader(b))
		if err != nil {
			return "", err
		}

		token := a.token()
		req.Header.Set("Authorization", "Bearer "+token)

		client := &http.Client{Timeout: apiRequestTimeout}
		res, err := client.Do(req)
		if err != nil {
			return "", err
		}

		// Handle 409 (Conflict), 429 (Too Many Requests), and 401 (Unauthorized)
		if res.StatusCode == 409 || res.StatusCode == 429 || res.StatusCode == 401 {
			res.Body.Close()
			if attempt < maxRetries-1 {
				// Get new token from Google
				if err := a.refreshToken(token); err != nil {
					return "", err
				}
				time.Sleep(retryDelay)
				retryDelay *= 2 // exponential backoff
				continue
			}
		}

		defer res.Body.Close()

		if res.StatusCode != 200 {
			return "", errors.New("nest: wrong status: " + res.Status)
		}

		var resv struct {
			Results struct {
				Answer         string    `json:"answerSdp"`
				ExpiresAt      time.Time `json:"expiresAt"`
				MediaSessionID string    `json:"mediaSessionId"`
			} `json:"results"`
		}

		if err = json.NewDecoder(res.Body).Decode(&resv); err != nil {
			return "", err
		}

		a.streamMu.Lock()
		a.StreamProjectID = projectID
		a.StreamDeviceID = deviceID
		a.StreamSessionID = resv.Results.MediaSessionID
		a.StreamExpiresAt = resv.Results.ExpiresAt
		a.streamGeneration++
		a.streamMu.Unlock()

		return resv.Results.Answer, nil
	}

	return "", errors.New("nest: max retries exceeded")
}

func (a *API) refreshToken(previous string) error {
	if a.auth == nil {
		return errors.New("nest: missing shared auth state")
	}
	a.auth.mu.Lock()
	defer a.auth.mu.Unlock()

	if a.auth.token != previous {
		return nil
	}
	return a.auth.refreshLocked()
}

func (a *API) ExtendStream() error {
	return a.extendStream(context.Background())
}

func (a *API) extendStream(ctx context.Context) error {
	a.streamMu.RLock()
	projectID := a.StreamProjectID
	deviceID := a.StreamDeviceID
	sessionID := a.StreamSessionID
	streamToken := a.StreamToken
	extensionToken := a.StreamExtensionToken
	generation := a.streamGeneration
	a.streamMu.RUnlock()

	var reqv struct {
		Command string `json:"command"`
		Params  struct {
			MediaSessionID       string `json:"mediaSessionId,omitempty"`
			StreamExtensionToken string `json:"streamExtensionToken,omitempty"`
		} `json:"params"`
	}

	if streamToken != "" {
		// RTSP
		reqv.Command = "sdm.devices.commands.CameraLiveStream.ExtendRtspStream"
		reqv.Params.StreamExtensionToken = extensionToken
	} else {
		// WebRTC
		reqv.Command = "sdm.devices.commands.CameraLiveStream.ExtendWebRtcStream"
		reqv.Params.MediaSessionID = sessionID
	}

	b, err := json.Marshal(reqv)
	if err != nil {
		return err
	}

	uri := "https://smartdevicemanagement.googleapis.com/v1/enterprises/" +
		projectID + "/devices/" + deviceID + ":executeCommand"
	req, err := http.NewRequestWithContext(ctx, "POST", uri, bytes.NewReader(b))
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+a.token())

	client := &http.Client{Timeout: apiRequestTimeout}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		return errors.New("nest: wrong status: " + res.Status)
	}

	var resv struct {
		Results struct {
			ExpiresAt            time.Time `json:"expiresAt"`
			MediaSessionID       string    `json:"mediaSessionId"`
			StreamExtensionToken string    `json:"streamExtensionToken"`
			StreamToken          string    `json:"streamToken"`
		} `json:"results"`
	}

	if err = json.NewDecoder(res.Body).Decode(&resv); err != nil {
		return err
	}

	a.streamMu.Lock()
	if a.streamGeneration == generation {
		a.StreamSessionID = resv.Results.MediaSessionID
		a.StreamExpiresAt = resv.Results.ExpiresAt
		a.StreamExtensionToken = resv.Results.StreamExtensionToken
		a.StreamToken = resv.Results.StreamToken
	}
	a.streamMu.Unlock()

	return nil
}

func (a *API) GenerateRtspStream(projectID, deviceID string) (string, error) {
	var reqv struct {
		Command string   `json:"command"`
		Params  struct{} `json:"params"`
	}
	reqv.Command = "sdm.devices.commands.CameraLiveStream.GenerateRtspStream"

	b, err := json.Marshal(reqv)
	if err != nil {
		return "", err
	}

	uri := "https://smartdevicemanagement.googleapis.com/v1/enterprises/" +
		projectID + "/devices/" + deviceID + ":executeCommand"
	req, err := http.NewRequest("POST", uri, bytes.NewReader(b))
	if err != nil {
		return "", err
	}

	req.Header.Set("Authorization", "Bearer "+a.token())

	client := &http.Client{Timeout: apiRequestTimeout}
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}

	if res.StatusCode != 200 {
		return "", errors.New("nest: wrong status: " + res.Status)
	}

	var resv struct {
		Results struct {
			StreamURLs           map[string]string `json:"streamUrls"`
			StreamExtensionToken string            `json:"streamExtensionToken"`
			StreamToken          string            `json:"streamToken"`
			ExpiresAt            time.Time         `json:"expiresAt"`
		} `json:"results"`
	}

	if err = json.NewDecoder(res.Body).Decode(&resv); err != nil {
		return "", err
	}

	if _, ok := resv.Results.StreamURLs["rtspUrl"]; !ok {
		return "", errors.New("nest: failed to generate rtsp url")
	}

	a.streamMu.Lock()
	a.StreamProjectID = projectID
	a.StreamDeviceID = deviceID
	a.StreamToken = resv.Results.StreamToken
	a.StreamExtensionToken = resv.Results.StreamExtensionToken
	a.StreamExpiresAt = resv.Results.ExpiresAt
	a.streamGeneration++
	a.streamMu.Unlock()

	return resv.Results.StreamURLs["rtspUrl"], nil
}

func (a *API) StopRTSPStream() error {
	a.streamMu.Lock()
	projectID, deviceID, extensionToken := a.StreamProjectID, a.StreamDeviceID, a.StreamExtensionToken
	if projectID == "" || deviceID == "" {
		a.streamMu.Unlock()
		return errors.New("nest: tried to stop rtsp stream without a project or device ID")
	}
	a.StreamProjectID = ""
	a.StreamDeviceID = ""
	a.StreamExtensionToken = ""
	a.StreamToken = ""
	a.streamGeneration++
	a.streamMu.Unlock()

	var reqv struct {
		Command string `json:"command"`
		Params  struct {
			StreamExtensionToken string `json:"streamExtensionToken"`
		} `json:"params"`
	}
	reqv.Command = "sdm.devices.commands.CameraLiveStream.StopRtspStream"
	reqv.Params.StreamExtensionToken = extensionToken

	b, err := json.Marshal(reqv)
	if err != nil {
		return err
	}

	uri := "https://smartdevicemanagement.googleapis.com/v1/enterprises/" +
		projectID + "/devices/" + deviceID + ":executeCommand"
	req, err := http.NewRequest("POST", uri, bytes.NewReader(b))
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+a.token())

	client := &http.Client{Timeout: apiRequestTimeout}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		return errors.New("nest: wrong status: " + res.Status)
	}

	return nil
}

func (a *API) StopWebRTCStream() error {
	a.streamMu.RLock()
	projectID, deviceID, sessionID := a.StreamProjectID, a.StreamDeviceID, a.StreamSessionID
	generation := a.streamGeneration
	if projectID == "" || deviceID == "" || sessionID == "" {
		a.streamMu.RUnlock()
		return errors.New("nest: tried to stop webrtc stream without a project, device, or session ID")
	}
	a.streamMu.RUnlock()

	var reqv struct {
		Command string `json:"command"`
		Params  struct {
			MediaSessionID string `json:"mediaSessionId"`
		} `json:"params"`
	}
	reqv.Command = "sdm.devices.commands.CameraLiveStream.StopWebRtcStream"
	reqv.Params.MediaSessionID = sessionID

	b, err := json.Marshal(reqv)
	if err != nil {
		return err
	}

	uri := "https://smartdevicemanagement.googleapis.com/v1/enterprises/" +
		projectID + "/devices/" + deviceID + ":executeCommand"
	req, err := http.NewRequest("POST", uri, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.token())

	client := &http.Client{Timeout: 5 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode != 200 && res.StatusCode != http.StatusNotFound {
		return errors.New("nest: wrong status: " + res.Status)
	}

	a.streamMu.Lock()
	if a.streamGeneration == generation {
		a.StreamProjectID = ""
		a.StreamDeviceID = ""
		a.StreamSessionID = ""
		a.StreamToken = ""
		a.StreamExtensionToken = ""
		a.StreamExpiresAt = time.Time{}
		a.streamGeneration++
	}
	a.streamMu.Unlock()
	return nil
}

type Device struct {
	Name string `json:"name"`
	Type string `json:"type"`
	//Assignee string `json:"assignee"`
	Traits struct {
		SdmDevicesTraitsInfo struct {
			CustomName string `json:"customName"`
		} `json:"sdm.devices.traits.Info"`
		SdmDevicesTraitsCameraLiveStream struct {
			VideoCodecs        []string `json:"videoCodecs"`
			AudioCodecs        []string `json:"audioCodecs"`
			SupportedProtocols []string `json:"supportedProtocols"`
		} `json:"sdm.devices.traits.CameraLiveStream"`
		//SdmDevicesTraitsCameraImage struct {
		//	MaxImageResolution struct {
		//		Width  int `json:"width"`
		//		Height int `json:"height"`
		//	} `json:"maxImageResolution"`
		//} `json:"sdm.devices.traits.CameraImage"`
		//SdmDevicesTraitsCameraPerson struct {
		//} `json:"sdm.devices.traits.CameraPerson"`
		//SdmDevicesTraitsCameraMotion struct {
		//} `json:"sdm.devices.traits.CameraMotion"`
		//SdmDevicesTraitsDoorbellChime struct {
		//} `json:"sdm.devices.traits.DoorbellChime"`
		//SdmDevicesTraitsCameraClipPreview struct {
		//} `json:"sdm.devices.traits.CameraClipPreview"`
	} `json:"traits"`
	ParentRelations []struct {
		Parent      string `json:"parent"`
		DisplayName string `json:"displayName"`
	} `json:"parentRelations"`
}

func (a *API) StartExtendStreamTimer() {
	a.streamMu.RLock()
	expiresAt := a.StreamExpiresAt
	a.streamMu.RUnlock()

	a.extendMu.Lock()
	if a.extendTimer != nil {
		a.extendMu.Unlock()
		return
	}

	timer := time.NewTimer(time.Until(expiresAt) - time.Minute)
	done := make(chan struct{})
	finished := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	a.extendTimer = timer
	a.extendDone = done
	a.extendFinished = finished
	a.extendCancel = cancel
	a.extendMu.Unlock()

	go func() {
		defer close(finished)
		select {
		case <-timer.C:
			select {
			case <-done:
				return
			default:
			}
			if a.extendRequest != nil {
				_ = a.extendRequest(ctx)
			} else {
				_ = a.extendStream(ctx)
			}
		case <-done:
			return
		}

		a.extendMu.Lock()
		if a.extendTimer == timer {
			a.extendTimer = nil
			a.extendDone = nil
			a.extendFinished = nil
			a.extendCancel = nil
		}
		a.extendMu.Unlock()
		cancel()
	}()
}

func (a *API) StopExtendStreamTimer() {
	a.extendMu.Lock()
	timer, done, finished, cancel := a.extendTimer, a.extendDone, a.extendFinished, a.extendCancel
	a.extendTimer = nil
	a.extendDone = nil
	a.extendFinished = nil
	a.extendCancel = nil
	a.extendMu.Unlock()
	if timer != nil {
		timer.Stop()
		close(done)
	}
	if cancel != nil {
		cancel()
	}
	if finished != nil {
		<-finished
	}
}
