package twitter

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/tidwall/gjson"
	"github.com/unkmonster/tmd2/internal/utils"
)

func Login(cookie_str string, authToken string) (*resty.Client, string, error) {
	cookie, err := utils.ParseCookie(cookie_str)
	if err != nil {
		return nil, "", err
	}

	client := resty.New()
	client.SetHeader("cookie", cookie_str)
	client.SetHeader("X-Csrf-Token", cookie["ct0"])
	client.SetAuthToken(authToken)
	client.SetRetryCount(5)
	client.AddRetryCondition(func(r *resty.Response, err error) bool {
		return !strings.HasSuffix(r.Request.RawRequest.Host, "twimg.com") && err != nil
	})
	client.SetTransport(&http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       5 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
	})
	client.SetTimeout(5 * time.Second)

	// 验证登录是否有效
	resp, err := client.R().Get("https://api.x.com/1.1/account/settings.json")
	if err != nil {
		return nil, "", err
	}
	if err = utils.CheckRespStatus(resp); err != nil {
		return nil, "", err
	}

	return client, gjson.Get(resp.String(), "screen_name").String(), nil
}

type xRateLimit struct {
	ResetTime time.Time
	Remaining int
	Limit     int
	Ready     bool
	Url       string
}

func (rl *xRateLimit) Req() bool {
	if !rl.Ready {
		log.Printf("not ready %s\n", rl.Url)
		return false
	}

	if time.Now().After(rl.ResetTime) {
		log.Printf("expired %s\n", rl.Url)
		rl.Ready = false
		return false
	}

	if rl.Remaining > rl.Limit/100 {
		rl.Remaining--
		//log.Printf("requested %s: remaining  %d\n", rl.Url, rl.Remaining)
		return true
	} else {
		log.Printf("[RateLimit] %s Sleep until %s\n", rl.Url, rl.ResetTime)
		time.Sleep(time.Until(rl.ResetTime))
		rl.Ready = false
		return false
	}
}

func makeRateLimit(resp *resty.Response) *xRateLimit {
	header := resp.Header()
	limit := header.Get("X-Rate-Limit-Limit")
	if limit == "" {
		return nil // 没有速率限制信息
	}
	remaining := header.Get("X-Rate-Limit-Remaining")
	if remaining == "" {
		return nil // 没有速率限制信息
	}
	resetTime := header.Get("X-Rate-Limit-Reset")
	if resetTime == "" {
		return nil // 没有速率限制信息
	}

	resetTimeNum, err := strconv.ParseUint(resetTime, 10, 64)
	if err != nil {
		return nil
	}
	remainingNum, err := strconv.Atoi(remaining)
	if err != nil {
		return nil
	}
	limitNum, err := strconv.Atoi(limit)
	if err != nil {
		return nil
	}

	u, _ := url.Parse(resp.Request.URL)
	url := filepath.Join(u.Host, u.Path)

	return &xRateLimit{
		ResetTime: time.Unix(int64(resetTimeNum), 0),
		Remaining: remainingNum,
		Limit:     limitNum,
		Ready:     true,
		Url:       url,
	}
}

type rateLimiter struct {
	limiters sync.Map
	conds    sync.Map
}

func (rateLimiter *rateLimiter) Check(url *url.URL) {
	if !rateLimiter.ShouldWork(url) {
		return
	}

	path := url.Path
	// 首次遇见某个路径时初始化它
	cod, _ := rateLimiter.conds.LoadOrStore(path, sync.NewCond(&sync.Mutex{}))
	cond := cod.(*sync.Cond)
	cond.L.Lock()
	defer cond.L.Unlock()

	// 首次遇见某个路径时初始化它
	// 但在响应头中如果获取不到速率限制信息将此键赋 nil
	lim, loaded := rateLimiter.limiters.LoadOrStore(path, &xRateLimit{})
	limiter := lim.(*xRateLimit)
	if !loaded {
		fmt.Printf("initial req: %s\n", path)
		return
	}

	// 路径过期后的首个请求可以正常发起，其余请求再次等待
	// 保证当前路径的速率限制会被另一个请求更新使其就绪，否则这里会无尽等待
	for limiter != nil && !limiter.Ready {
		fmt.Printf("wait for ready: %s\n", path)
		cond.Wait()
		lim, loaded := rateLimiter.limiters.LoadOrStore(path, &xRateLimit{})
		if !loaded {
			// 上个请求失败了，从它身上继承更新速率限制的重任
			fmt.Printf("inherited initial req: %s\n", path)
			return
		}
		limiter = lim.(*xRateLimit)
	}

	// limiter 为 nil 意味着不对此路径做速率限制，否则必须等待至速率限制信息准备就绪
	if limiter != nil {
		limiter.Req()
	}
	fmt.Printf("start req: %s\n", path)
}

func (rateLimiter *rateLimiter) Update(resp *resty.Response) {
	if !rateLimiter.ShouldWork(resp.RawResponse.Request.URL) {
		return
	}

	path := resp.RawResponse.Request.URL.Path

	co, _ := rateLimiter.conds.Load(path)
	cond := co.(*sync.Cond)
	cond.L.Lock()
	defer cond.L.Unlock()

	lim, _ := rateLimiter.limiters.Load(path) // 一定能加载到一个值
	limiter := lim.(*xRateLimit)
	if limiter == nil || limiter.Ready {
		return
	}

	// 重置速率限制
	newLimiter := makeRateLimit(resp)
	rateLimiter.limiters.Store(path, newLimiter)
	cond.Broadcast()
	fmt.Printf("updated: %s\n", path)
}

func (rateLimiter *rateLimiter) Reset(url *url.URL) {
	if !rateLimiter.ShouldWork(url) {
		return
	}

	path := url.Path
	co, ok := rateLimiter.conds.Load(path)
	if !ok {
		return
	}
	cond := co.(*sync.Cond)
	cond.L.Lock()
	defer cond.L.Unlock()

	lim, ok := rateLimiter.limiters.Load(path) // 一定能加载到一个值
	if !ok {
		// OnError 但是已被 OnRetry 重置
		return
	}
	limiter := lim.(*xRateLimit)
	if limiter == nil || limiter.Ready {
		return
	}

	// 将此路径设为首次请求的状态
	rateLimiter.limiters.Delete(path)
	cond.Signal()
	fmt.Printf("reseted: %s\n", path)
}

func (*rateLimiter) ShouldWork(url *url.URL) bool {
	return !strings.HasSuffix(url.Host, "twimg.com")
}

// 在 client.RetryCount 不为0的情况下
// 每次请求 retryHook 和 respHook 仅有一个被调用

func EnableRateLimit(client *resty.Client) {
	rateLimiter := rateLimiter{}

	client.OnBeforeRequest(func(c *resty.Client, req *resty.Request) error {
		u, err := url.Parse(req.URL)
		if err != nil {
			panic(err)
		}
		rateLimiter.Check(u)
		return nil
	})

	client.OnAfterResponse(func(c *resty.Client, resp *resty.Response) error {
		rateLimiter.Update(resp)
		return nil
	})

	client.AddRetryHook(func(resp *resty.Response, _ error) {
		if resp == nil {
			// 请求未发起 (Http.Client.Do 未被调用)
			return
		}
		rateLimiter.Reset(resp.Request.RawRequest.URL)
	})

	client.OnError(func(req *resty.Request, _ error) {
		rateLimiter.Reset(req.RawRequest.URL)
	})
}
