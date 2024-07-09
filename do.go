package main

type DoResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Ttl     int    `json:"ttl"`
}

func NewDoResponse() *DoResponse {
	return &DoResponse{
		Code: -999,
	}
}
