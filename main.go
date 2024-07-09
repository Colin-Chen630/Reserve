package main

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/imroc/req/v3"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var Cookie string

var UA = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5_1 like Mac OS X) AppleWebKit/618.2.12.10.9 (KHTML, like Gecko) Mobile/21F90 BiliApp/80300100 os/ios model/iPhone 14 Pro Max mobi_app/iphone build/80300100 osVer/17.5.1 network/2 channel/AppStore Buvid/${BUVID} c_locale/zh-Hans_CN s_locale/zh-Hans_JP sessionID/11fa54f6 disable_rcmd/0"

var InfoUrl = "https://api.bilibili.com/x/activity/bws/online/park/reserve/info?csrf=${csrf}&reserve_date=20240712,20240713,20240714"

const DoUrl = "https://api.bilibili.com/x/activity/bws/online/park/reserve/do"

// Proxy Working Policy:
// 1. If you want to use proxy, set the proxy in config.json
// 2. Policy only triggerd while received and "too many requests" error for one time.
var Proxy string

var logger *zap.Logger
var nameMap map[int]string

var ReservationMap = map[int]*InfoReserveDetail{}

var currentTimeOffset time.Duration
var TicketData = map[string]InfoTicketInfo{}
var ch = make(chan *req.Response)
var lock sync.Mutex

// TargetPair Reserve ID: ticket ID
var TargetPair = map[int]string{}

func InitLogger() {
	config := zap.NewDevelopmentConfig()
	config.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	l, _ := config.Build()
	logger = l
}

func GetReservationInfo() (*InfoResponse, error) {
	var client = req.C().SetUserAgent(UA).SetTLSFingerprintIOS().ImpersonateSafari()
	var result InfoResponse
	_, err := client.R().
		SetHeader("Cookie", Cookie).
		SetSuccessResult(&result).Get(InfoUrl)
	if err != nil {
		logger.Error("获取Info接口错误", zap.Error(err))
		return nil, err
	}
	if result.Code != 0 {
		logger.Error("Info 返回不为0", zap.String("message", result.Message))
		return nil, err
	}

	return &result, nil
}

func GetUserTicketInfo(info *InfoData) {
	for _, ticket := range info.UserTicketInfo {
		logger.Info("当前可用票", zap.String("票别", ticket.SkuName), zap.String("票号", ticket.Ticket), zap.String("日期", ticket.ScreenName))
		TicketData[ticket.Ticket] = ticket
	}
}

func writeAllResponseToFile() {
	//init a writer
	f, err := os.Create("response.txt")
	if err != nil {
		logger.Error("读写文件错误", zap.Error(err))
	}
	defer f.Close()
	for {
		select {
		case r := <-ch:
			_, err := f.WriteString(r.String() + "\n")
			if err != nil {
				logger.Error("写文件错误", zap.Error(err))
			}
		}
	}

}

func CallReserve(csrf string, reserveId int, ticketNo string, client *req.Client) (*DoResponse, error) {
	var result = NewDoResponse()
	body := "csrf=" + csrf +
		"&inter_reserve_id=" + strconv.Itoa(reserveId) +
		"&ticket_no=" + ticketNo
	resp, err := client.R().
		SetHeader("cookie", Cookie).
		SetHeader("content-type", "application/x-www-form-urlencoded").
		SetHeader("referer", "https://www.bilibili.com/blackboard/bw/2024/bws_event.html?navhide=1&stahide=1&native.theme=2&night=1#/Order/FieldOrder").
		SetSuccessResult(&result).
		SetBody(body).Post(DoUrl)
	ch <- resp
	if err != nil {
		if resp != nil && resp.StatusCode == 429 {
			logger.Error("429 - 请求频率过高", zap.Error(err))
			return nil, err
		}
		if resp != nil && resp.StatusCode == 412 {
			logger.Error("412 - STATUS CODE", zap.Error(err))
			return nil, err
		}
		logger.Error("获取Do接口错误", zap.Error(err))
		return nil, err
	}

	return result, nil
}

func GetCSRFFromCookie(cookie string) string {
	//Split the cookie
	cookieArray := strings.Split(cookie, ";")
	for _, c := range cookieArray {
		if strings.Contains(c, "bili_jct") {
			return strings.Split(c, "=")[1]
		}
	}
	logger.Error("未找到CSRF Token")
	return ""
}

func getReservationStartDate(info InfoData, reserveId int) (int64, error) {
	for _, value := range info.ReserveList {
		for _, v := range value {
			if v.ReserveID == reserveId {
				if v.NextReserve.ReserveBeginTime != 0 {
					return v.NextReserve.ReserveBeginTime, nil
				} else {
					return v.ReserveBeginTime, nil
				}
			}
		}
	}
	return -1, errors.New("未找到预约信息")
}

func isVIPTicket(ticketNo string) bool {
	ticket := TicketData[ticketNo]
	return strings.Contains(ticket.SkuName, "VIP")

}

func mapAllReserveInfo(info *InfoData) {
	for _, value := range info.ReserveList {
		for _, v := range value {
			ReservationMap[v.ReserveID] = &v
		}
	}
}

func checkEagiblity(reserveId int, ticketNo string) bool {
	show := ReservationMap[reserveId]
	//check is vip
	ticket := TicketData[ticketNo]
	if show.NextReserve.ReserveBeginTime > 0{
		if show.NextReserve.IsVipTicket == 1 && !isVIPTicket(ticketNo) {
			logger.Error(nameMap[reserveId] + " @ " + ticket.ScreenName + " - 下次预约要求为VIP限定，不符合要求。")
			return false
		}
	}
	//下次预约是否为VIP票限定
	if show.IsVipTicket == 1 && !isVIPTicket(ticketNo) {
		logger.Error(nameMap[reserveId] + " @ " + ticket.ScreenName + " - 预约要求为VIP限定，不符合要求。")
		return false
	}
	return true
}

func createReservationJob(reserveId int, ticketNo string, csrfToken string, info InfoData, wg *sync.WaitGroup) {
	reservationStartDate, err := getReservationStartDate(info, reserveId)
	if err != nil {
		logger.Error("无法获取预约开始时间", zap.Error(err))
	}
	if !checkEagiblity(reserveId, ticketNo) {
		logger.Error("不符合要求，退出任务。")
		return
	}
	go doReserve(reservationStartDate, reserveId, ticketNo, csrfToken, wg)

}

func doReserve(startTime int64, reservedId int, ticketId string, csrfToken string, wg *sync.WaitGroup) {
	defer wg.Done()
	//calculate the timer decay
	realStartTime := startTime * 1000
	ticket := TicketData[ticketId]
	var client = req.C().SetUserAgent(UA).SetTLSFingerprintIOS().ImpersonateSafari()
	usingProxy := false
	for {
		//get start time
		currentTime := time.Now().Add(currentTimeOffset).UnixMilli()
		timeDifference := realStartTime - currentTime
		if timeDifference > 0 {
			// wait for half of the difference
			waitFor := timeDifference / 2
			logger.Info(nameMap[reservedId]+" @ "+ticket.ScreenName+" - 等待预约开始", zap.Time("开始时间", time.Unix(startTime, 0)), zap.Time("当前时间", time.UnixMilli(currentTime)), zap.Duration("时间偏移", currentTimeOffset), zap.Any("等待", time.Duration(timeDifference)*time.Millisecond/2))
			time.Sleep(time.Duration(waitFor) * time.Millisecond)
			continue
		}
		//do reserve
		lock.Lock()
		reservation, err := CallReserve(csrfToken, reservedId, ticketId, client)
		if usingProxy {
			client = client.SetProxy(nil)
		}
		if err != nil {
			logger.Error(nameMap[reservedId]+" @"+ticket.ScreenName+" - 预约失败，内部错误，重试中。", zap.Error(err))
			lock.Unlock()
			continue
		}
		if reservation.Code != 0 {
			switch reservation.Code {
			case 412:
			case 429:
				logger.Error(nameMap[reservedId]+" @ "+ticket.ScreenName+" - 412 / 429 重试中, 账号/IP 可能被限制。", zap.String("message", reservation.Message))
				if Proxy != "" && !usingProxy {
					lock.Unlock()
					client = client.SetProxyURL(Proxy)
					logger.Info(nameMap[reservedId] + " @ " + ticket.ScreenName + " - 已经切换代理模式。")
					usingProxy = true
					continue
				}
				usingProxy = false
				time.Sleep(500 * time.Millisecond)
				lock.Unlock()
				continue
			case 76650:
				logger.Error(nameMap[reservedId]+" @ "+ticket.ScreenName+" - 操作频繁，等待重试。", zap.String("message", reservation.Message))
				if Proxy != "" && !usingProxy {
					lock.Unlock()
					client = client.SetProxyURL(Proxy)
					logger.Info(nameMap[reservedId] + " @ " + ticket.ScreenName + " - 已经切换代理模式。")
					usingProxy = true
					continue
				}
				time.Sleep(500 * time.Millisecond)
				usingProxy = false
				lock.Unlock()
				continue
			case 76647:
				logger.Error(nameMap[reservedId]+" @ "+ticket.ScreenName+" - 该账户预约次数达到上限，退出此任务。", zap.String("message", reservation.Message))
				lock.Unlock()
				return
			case -702:
				logger.Error(nameMap[reservedId]+" @ "+ticket.ScreenName+" - 请求频率过高，等待重试。", zap.String("message", reservation.Message))
				if Proxy != "" && !usingProxy {
					lock.Unlock()
					client = client.SetProxyURL(Proxy)
					logger.Info(nameMap[reservedId] + " @ " + ticket.ScreenName + " - 已经切换代理模式。")
					usingProxy = true
					continue
				}
				time.Sleep(500 * time.Millisecond)
				usingProxy = false
				lock.Unlock()
				continue
			case 75574:
				logger.Error(nameMap[reservedId]+" @ "+ticket.ScreenName+" - 本项目预约已满。不存在回流机制，结束任务。", zap.String("message", reservation.Message))
				lock.Unlock()
				return
			case 75637:
				logger.Error(nameMap[reservedId]+" @ "+ticket.ScreenName+" - 项目可能未开始！紧急重试！", zap.String("message", reservation.Message))
				if Proxy != "" && !usingProxy {
					lock.Unlock()
					logger.Info(nameMap[reservedId] + " @ " + ticket.ScreenName + " - 已经切换代理模式。")
					client = client.SetProxyURL(Proxy)
					usingProxy = true
					continue
				}
				time.Sleep(500 * time.Millisecond)
				usingProxy = false
				lock.Unlock()
				continue
			default:
				logger.Error(nameMap[reservedId]+" @ "+ticket.ScreenName+" - 警告：未知返回代码！防止风控，立即结束当前任务！", zap.String("message", reservation.Message), zap.Any("code", reservation.Code))
				lock.Unlock()
				return
			}
		}
		if reservation.Message != "0" || reservation.Code == -999 {
			logger.Error(nameMap[reservedId] + " @ " + ticket.ScreenName + "返回数据反序列化失败。重新请求。")
			time.Sleep(500 * time.Millisecond)
			lock.Unlock()
			continue
		}
		logger.Info(nameMap[reservedId]+" @ "+ticket.ScreenName+" - 预约成功,请检查右侧 message 是否是 0。", zap.String("message", reservation.Message))
		time.Sleep(450 * time.Millisecond)
		lock.Unlock()
		return
	}
}

func createReservationIDandNameMap(info InfoData) {
	result := make(map[int]string)
	for _, value := range info.ReserveList {
		for _, v := range value {
			result[v.ReserveID] = v.ActTitle
		}
	}
	nameMap = result
}

func syncTimeOffset() {
	timeOffset, err := GetNTPOffset()
	if err != nil {
		logger.Error("获取时间失败", zap.Error(err))
	}
	if timeOffset != nil {
		logger.Info("当前时间偏移", zap.Duration("时间偏移", *timeOffset))
		currentTimeOffset = *timeOffset
	} else {
		logger.Warn("未获取到时间偏移")
	}

}
func main() {
	InitLogger()
	logger.Info("程序已启动。")
	var configFile string
	//load args
	if len(os.Args) > 1 {
		configFile = os.Args[1]
	}
	if configFile == "" {
		configFile = "config.json"
	}
	config, err := LoadConfig(configFile)
	if err != nil {
		logger.Error("无法加载配置文件", zap.Error(err))
		return
	}
	logger.Info("配置文件已加载", zap.String("文件", configFile))
	Cookie = config.Cookie
	csrfToken := GetCSRFFromCookie(Cookie)
	if csrfToken == "" {
		logger.Error("获取CSRF Token失败")
		return
	}
	logger.Info("CSRF Token", zap.String("token", csrfToken))
	//set buvid in ua
	UA = strings.ReplaceAll(UA, "${BUVID}", config.BuvID)
	//set proxy
	if config.Proxy != "" {
		Proxy = config.Proxy
		logger.Info("代理设置被添加", zap.String("proxy", Proxy))
	}
	//set csrf token
	InfoUrl = strings.ReplaceAll(InfoUrl, "${csrf}", csrfToken)

	logger.Info("用户 UA", zap.String("UA", UA))
	TargetPair = convertJobKeyType(config.Job)
	timeOffset, err := GetNTPOffset()
	if err != nil {
		logger.Error("获取时间失败", zap.Error(err))
	}
	if timeOffset != nil {
		logger.Info("当前时间偏移", zap.Duration("时间偏移", *timeOffset))
		currentTimeOffset = *timeOffset
	} else {
		logger.Warn("未获取到时间偏移")
	}

	// 获取预约信息
	info, err := GetReservationInfo()
	if err != nil {
		logger.Error("获取预约信息失败", zap.Error(err))
		return
	}
	go writeAllResponseToFile()
	// 获取用户可用票
	GetUserTicketInfo(&info.Data)
	createReservationIDandNameMap(info.Data)
	mapAllReserveInfo(&info.Data)
	var wg sync.WaitGroup
	//set up time sync
	syncTimeOffset()
	//测试预约
	//resp, err := CallReserve(csrfToken, 6016, "15111332527932")
	//if err != nil {
	//	println(err)
	//	return
	//}
	//print resp code and message
	//logger.Info("预约结果", zap.Int("code", resp.Code), zap.String("message", resp.Message))
	// 预约

	for reserveId, ticketNo := range TargetPair {
		wg.Add(1)
		createReservationJob(reserveId, ticketNo, csrfToken, info.Data, &wg)
	}
	wg.Wait()
	logger.Info("所有任务已完成。")
}
