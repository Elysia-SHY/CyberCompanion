package qq

type WSPayload struct {
	Op int         `json:"op"`
	D  interface{} `json:"d,omitempty"`
	S  *int64      `json:"s,omitempty"`
	T  string      `json:"t,omitempty"`
}

type WSHelloData struct {
	HeartbeatInterval int `json:"heartbeat_interval"`
}

type WSIdentifyData struct {
	Token      string `json:"token"`
	Intents    int    `json:"intents"`
	Shard      []int  `json:"shard"`
	Properties struct {
		OS      string `json:"$os"`
		Browser string `json:"$browser"`
		Device  string `json:"$device"`
	} `json:"properties"`
}

type Attachment struct {
	ContentType string `json:"content_type"`
	Filename    string `json:"filename"`
	Height      int    `json:"height"`
	Width       int    `json:"width"`
	Size        int    `json:"size"`
	URL         string `json:"url"`
}

type Author struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	Avatar       string `json:"avatar"`
	Bot          bool   `json:"bot"`
	UserOpenID   string `json:"user_openid"`
	MemberOpenID string `json:"member_openid"`
}

type InMessage struct {
	ID          string       `json:"id"`
	Content     string       `json:"content"`
	Timestamp   string       `json:"timestamp"`
	Author      Author       `json:"author"`
	Attachments []Attachment `json:"attachments"`
	GroupOpenID string       `json:"group_openid"`
}

type PostMessageReq struct {
	Content string `json:"content,omitempty"`
	MsgType int    `json:"msg_type"` // 0: text, 7: media
	MsgID   string `json:"msg_id,omitempty"`
}

type SendMediaFileReq struct {
	FileType   int    `json:"file_type"` // 1: image
	URL        string `json:"url,omitempty"`
	SrvSendMsg bool   `json:"srv_send_msg"`
	FileData   string `json:"file_data,omitempty"`
}
