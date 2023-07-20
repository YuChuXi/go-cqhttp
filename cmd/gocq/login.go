package gocq

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"fmt"
	"image"
	"image/png"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Mrs4s/MiraiGo/client"
	"github.com/Mrs4s/MiraiGo/utils"
	"github.com/mattn/go-colorable"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"gopkg.ilharper.com/x/isatty"

	"github.com/Mrs4s/go-cqhttp/internal/base"

	"github.com/Mrs4s/go-cqhttp/global"
	"github.com/Mrs4s/go-cqhttp/internal/download"
)

var console = bufio.NewReader(os.Stdin)

func readLine() (str string) {
	str, _ = console.ReadString('\n')
	str = strings.TrimSpace(str)
	return
}

func readLineTimeout(t time.Duration) {
	r := make(chan string)
	go func() {
		select {
		case r <- readLine():
		case <-time.After(t):
		}
	}()
	select {
	case <-r:
	case <-time.After(t):
	}
}

func readIfTTY(de string) (str string) {
	if isatty.Isatty(os.Stdin.Fd()) {
		return readLine()
	}
	log.Warnf("未检测到输入终端，自动选择%s.", de)
	return de
}

var cli *client.QQClient
var device *client.DeviceInfo

// ErrSMSRequestError SMS请求出错
var ErrSMSRequestError = errors.New("sms request error")

func commonLogin() error {
	res, err := cli.Login()
	if err != nil {
		return err
	}
	return loginResponseProcessor(res)
}

func printQRCode(imgData []byte) {
	const (
		black = "\033[48;5;0m  \033[0m"
		white = "\033[48;5;7m  \033[0m"
	)
	img, err := png.Decode(bytes.NewReader(imgData))
	if err != nil {
		log.Panic(err)
	}
	data := img.(*image.Gray).Pix
	bound := img.Bounds().Max.X
	buf := make([]byte, 0, (bound*4+1)*(bound))
	i := 0
	for y := 0; y < bound; y++ {
		i = y * bound
		for x := 0; x < bound; x++ {
			if data[i] != 255 {
				buf = append(buf, white...)
			} else {
				buf = append(buf, black...)
			}
			i++
		}
		buf = append(buf, '\n')
	}
	_, _ = colorable.NewColorableStdout().Write(buf)
}

func qrcodeLogin() error {
	rsp, err := cli.FetchQRCodeCustomSize(1, 2, 1)
	if err != nil {
		return err
	}
	_ = os.WriteFile("qrcode.png", rsp.ImageData, 0o644)
	defer func() { _ = os.Remove("qrcode.png") }()
	if cli.Uin != 0 {
		log.Infof("请使用账号 %v 登录手机QQ扫描二维码 (qrcode.png) : ", cli.Uin)
	} else {
		log.Infof("请使用手机QQ扫描二维码 (qrcode.png) : ")
	}
	time.Sleep(time.Second)
	printQRCode(rsp.ImageData)
	s, err := cli.QueryQRCodeStatus(rsp.Sig)
	if err != nil {
		return err
	}
	prevState := s.State
	for {
		time.Sleep(time.Second)
		s, _ = cli.QueryQRCodeStatus(rsp.Sig)
		if s == nil {
			continue
		}
		if prevState == s.State {
			continue
		}
		prevState = s.State
		switch s.State {
		case client.QRCodeCanceled:
			log.Fatalf("扫码被用户取消.")
		case client.QRCodeTimeout:
			log.Fatalf("二维码过期")
		case client.QRCodeWaitingForConfirm:
			log.Infof("扫码成功, 请在手机端确认登录.")
		case client.QRCodeConfirmed:
			res, err := cli.QRCodeLogin(s.LoginInfo)
			if err != nil {
				return err
			}
			return loginResponseProcessor(res)
		case client.QRCodeImageFetch, client.QRCodeWaitingForScan:
			// ignore
		}
	}
}

func loginResponseProcessor(res *client.LoginResponse) error {
	var err error
	for {
		if err != nil {
			return err
		}
		if res.Success {
			return nil
		}
		var text string
		switch res.Error {
		case client.SliderNeededError:
			log.Warnf("登录需要滑条验证码, 请验证后重试.")
			ticket := getTicket(res.VerifyUrl)
			if ticket == "" {
				log.Infof("按 Enter 继续....")
				readLine()
				os.Exit(0)
			}
			res, err = cli.SubmitTicket(ticket)
			continue
		case client.NeedCaptcha:
			log.Warnf("登录需要验证码.")
			_ = os.WriteFile("captcha.jpg", res.CaptchaImage, 0o644)
			log.Warnf("请输入验证码 (captcha.jpg)： (Enter 提交)")
			text = readLine()
			global.DelFile("captcha.jpg")
			res, err = cli.SubmitCaptcha(text, res.CaptchaSign)
			continue
		case client.SMSNeededError:
			log.Warnf("账号已开启设备锁, 按 Enter 向手机 %v 发送短信验证码.", res.SMSPhone)
			readLine()
			if !cli.RequestSMS() {
				log.Warnf("发送验证码失败，可能是请求过于频繁.")
				return errors.WithStack(ErrSMSRequestError)
			}
			log.Warn("请输入短信验证码： (Enter 提交)")
			text = readLine()
			res, err = cli.SubmitSMS(text)
			continue
		case client.SMSOrVerifyNeededError:
			log.Warnf("账号已开启设备锁，请选择验证方式:")
			log.Warnf("1. 向手机 %v 发送短信验证码", res.SMSPhone)
			log.Warnf("2. 使用手机QQ扫码验证.")
			log.Warn("请输入(1 - 2)：")
			text = readIfTTY("2")
			if strings.Contains(text, "1") {
				if !cli.RequestSMS() {
					log.Warnf("发送验证码失败，可能是请求过于频繁.")
					return errors.WithStack(ErrSMSRequestError)
				}
				log.Warn("请输入短信验证码： (Enter 提交)")
				text = readLine()
				res, err = cli.SubmitSMS(text)
				continue
			}
			fallthrough
		case client.UnsafeDeviceError:
			log.Warnf("账号已开启设备锁，请前往 -> %v <- 验证后重启Bot.", res.VerifyUrl)
			log.Infof("按 Enter 或等待 5s 后继续....")
			readLineTimeout(time.Second * 5)
			os.Exit(0)
		case client.OtherLoginError, client.UnknownLoginError, client.TooManySMSRequestError:
			msg := res.ErrorMessage
			log.Warnf("登录失败: %v Code: %v", msg, res.Code)
			switch res.Code {
			case 235:
				log.Warnf("设备信息被封禁, 请删除 device.json 后重试.")
			case 237:
				log.Warnf("登录过于频繁, 请在手机QQ登录并根据提示完成认证后等一段时间重试")
			case 45:
				log.Warnf("你的账号被限制登录, 请配置 SignServer 后重试")
			}
			log.Infof("按 Enter 继续....")
			readLine()
			os.Exit(0)
		}
	}
}

func getTicket(u string) string {
	log.Warnf("请选择提交滑块ticket方式:")
	log.Warnf("1. 自动提交")
	log.Warnf("2. 手动抓取提交")
	log.Warn("请输入(1 - 2)：")
	text := readLine()
	id := utils.RandomString(8)
	auto := !strings.Contains(text, "2")
	if auto {
		u = strings.ReplaceAll(u, "https://ssl.captcha.qq.com/template/wireless_mqq_captcha.html?", fmt.Sprintf("https://captcha.go-cqhttp.org/captcha?id=%v&", id))
	}
	log.Warnf("请前往该地址验证 -> %v ", u)
	if !auto {
		log.Warn("请输入ticket： (Enter 提交)")
		return readLine()
	}

	for count := 120; count > 0; count-- {
		str := fetchCaptcha(id)
		if str != "" {
			return str
		}
		time.Sleep(time.Second)
	}
	log.Warnf("验证超时")
	return ""
}

func fetchCaptcha(id string) string {
	g, err := download.Request{URL: "https://captcha.go-cqhttp.org/captcha/ticket?id=" + id}.JSON()
	if err != nil {
		log.Debugf("获取 Ticket 时出现错误: %v", err)
		return ""
	}
	if g.Get("ticket").Exists() {
		return g.Get("ticket").String()
	}
	return ""
}

func energy(uin uint64, id string, _ string, salt []byte) ([]byte, error) {
	signServer := base.SignServer
	if !strings.HasSuffix(signServer, "/") {
		signServer += "/"
	}
	req := download.Request{
		Method: http.MethodGet,
		URL: signServer + "custom_energy" + fmt.Sprintf("?data=%v&salt=%v&uin=%v&android_id=%v&guid=%v",
			id, hex.EncodeToString(salt), uin, hex.EncodeToString(device.AndroidId), hex.EncodeToString(device.Guid)),
	}
	if base.IsBelow110 {
		req.URL = signServer + "custom_energy" + fmt.Sprintf("?data=%v&salt=%v", id, hex.EncodeToString(salt))
	}
	response, err := req.Bytes()
	if err != nil {
		log.Warnf("获取T544 sign时出现错误: %v server: %v", err, signServer)
		return nil, err
	}
	data, err := hex.DecodeString(gjson.GetBytes(response, "data").String())
	if err != nil {
		log.Warnf("获取T544 sign时出现错误: %v", err)
		return nil, err
	}
	if len(data) == 0 {
		log.Warnf("获取T544 sign时出现错误: %v", "data is empty")
		return nil, errors.New("data is empty")
	}
	return data, nil
}

func sign(seq uint64, uin string, cmd string, qua string, buff []byte) (sign []byte, extra []byte, token []byte, err error) {
	signServer := base.SignServer
	if !strings.HasSuffix(signServer, "/") {
		signServer += "/"
	}
	response, err := download.Request{
		Method: http.MethodPost,
		URL:    signServer + "sign"+fmt.Sprintf("?),
		Header: map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
		Body:   bytes.NewReader([]byte(fmt.Sprintf("uin=%v&qua=%s&cmd=%s&seq=%v&buffer=%v", uin, qua, cmd, seq, hex.EncodeToString(buff)))),
	}.Bytes()
	if err != nil {
		log.Warnf("获取sso sign时出现错误: %v server: %v", err, signServer)
		return nil, nil, nil, err
	}
	sign, _ = hex.DecodeString(gjson.GetBytes(response, "data.sign").String())
	extra, _ = hex.DecodeString(gjson.GetBytes(response, "data.extra").String())
	token, _ = hex.DecodeString(gjson.GetBytes(response, "data.token").String())
	return sign, extra, token, nil
}

func register(uin int64, androidID, guid []byte, qimei36, key string) {
	if base.IsBelow110 {
		log.Warn("签名服务器版本低于1.1.0, 跳过实例注册")
		return
	}
	signServer := base.SignServer
	if !strings.HasSuffix(signServer, "/") {
		signServer += "/"
	}
	resp, err := download.Request{
		Method: http.MethodGet,
		URL: signServer + "register" + fmt.Sprintf("?uin=%v&android_id=%v&guid=%v&qimei36=%v&key=%s",
			uin, hex.EncodeToString(androidID), hex.EncodeToString(guid), qimei36, key),
	}.Bytes()
	if err != nil {
		log.Warnf("注册QQ实例时出现错误: %v server: %v", err, signServer)
		return
	}
	msg := gjson.GetBytes(resp, "msg")
	if gjson.GetBytes(resp, "code").Int() != 0 {
		log.Warnf("注册QQ实例时出现错误: %v server: %v", msg, signServer)
		return
	}
	log.Infof("注册QQ实例 %v 成功: %v", uin, msg)
}

func listenPort(wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		err := dailPort()
		if err != nil {
			log.Warnf("连接到签名服务器出现错误: %v", err)
			// 连接失败，每5秒检测一次
			time.Sleep(5 * time.Second)
		} else {
			break
		}
	}
}

func dailPort() error {
	u, err := url.Parse(base.SignServer)
	if err != nil {
		return err
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

func waitForConnection() {
	var wg sync.WaitGroup
	wg.Add(1)
	go listenPort(&wg)
	wg.Wait()
	log.Infof("连接至签名服务器: %s", base.SignServer)
}
