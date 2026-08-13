# Google Nest

[`new in v1.6.0`](https://github.com/AlexxIT/go2rtc/releases/tag/v1.6.0)

For simplicity, it is recommended to connect the Nest/WebRTC camera to the [Home Assistant](../hass/README.md). 
But if you can somehow get the below parameters, Nest/WebRTC source will work without Home Assistant.

```yaml
streams:
  nest-doorbell: nest:?client_id=***&client_secret=***&refresh_token=***&project_id=***&device_id=***
```

The stream renews 30 seconds before Google's `expiresAt` time. Set `renew_before`,
using a duration greater than 0 and less than 5 minutes, to renew earlier or later:

```yaml
streams:
  nest-doorbell: nest:?client_id=***&client_secret=***&refresh_token=***&project_id=***&device_id=***&renew_before=45s
```
