package ratelimit

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const redisTokenBucketScript = `local d=redis.call('HMGET',KEYS[1],'tokens','ts');local t=tonumber(d[1]);local ts=tonumber(d[2]);local rate=tonumber(ARGV[1]);local burst=tonumber(ARGV[2]);local now=tonumber(ARGV[3]);if t==nil then t=burst end;if ts==nil then ts=now end;local delta=math.max(0,now-ts)/1000.0;t=math.min(burst,t+delta*rate);local allowed=0;if t>=1 then t=t-1;allowed=1 end;redis.call('HSET',KEYS[1],'tokens',t,'ts',now);redis.call('PEXPIRE',KEYS[1],math.ceil((burst/rate)*2000));return allowed`

type redisConn struct {
	net.Conn
	r *bufio.Reader
}
type RedisLimiter struct {
	addr, password string
	tls            bool
	db             int
	timeout        time.Duration
	pool           chan *redisConn
	mu             sync.Mutex
	closed         bool
}

func NewRedis(addr, password string, db int) *RedisLimiter {
	useTLS := false
	if strings.HasPrefix(addr, "redis://") || strings.HasPrefix(addr, "rediss://") {
		u, err := url.Parse(addr)
		if err == nil {
			useTLS = u.Scheme == "rediss"
			addr = u.Host
			if password == "" && u.User != nil {
				password, _ = u.User.Password()
			}
			if p := strings.TrimPrefix(u.Path, "/"); p != "" {
				if parsed, err := strconv.Atoi(p); err == nil {
					db = parsed
				}
			}
		}
	}
	return &RedisLimiter{addr: addr, password: password, db: db, tls: useTLS, timeout: 2 * time.Second, pool: make(chan *redisConn, 16)}
}
func (r *RedisLimiter) dial(ctx context.Context) (*redisConn, error) {
	var c net.Conn
	var err error
	dialer := &net.Dialer{Timeout: r.timeout}
	if r.tls {
		c, err = (&tls.Dialer{NetDialer: dialer, Config: &tls.Config{MinVersion: tls.VersionTLS12}}).DialContext(ctx, "tcp", r.addr)
	} else {
		c, err = dialer.DialContext(ctx, "tcp", r.addr)
	}
	if err != nil {
		return nil, err
	}
	rc := &redisConn{Conn: c, r: bufio.NewReader(c)}
	if r.password != "" {
		if _, err = r.command(rc, "AUTH", r.password); err != nil {
			c.Close()
			return nil, err
		}
	}
	if r.db != 0 {
		if _, err = r.command(rc, "SELECT", strconv.Itoa(r.db)); err != nil {
			c.Close()
			return nil, err
		}
	}
	return rc, nil
}
func (r *RedisLimiter) get(ctx context.Context) (*redisConn, error) {
	select {
	case c := <-r.pool:
		return c, nil
	default:
		return r.dial(ctx)
	}
}
func (r *RedisLimiter) put(c *redisConn, ok bool) {
	if c == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !ok || r.closed {
		_ = c.Close()
		return
	}
	select {
	case r.pool <- c:
	default:
		_ = c.Close()
	}
}
func (r *RedisLimiter) Ping(ctx context.Context) error {
	c, err := r.get(ctx)
	if err != nil {
		return err
	}
	_, err = r.command(c, "PING")
	r.put(c, err == nil)
	return err
}
func (r *RedisLimiter) Allow(ctx context.Context, key string, rate float64, burst int) (bool, error) {
	c, err := r.get(ctx)
	if err != nil {
		return false, err
	}
	_ = c.SetDeadline(time.Now().Add(r.timeout))
	v, err := r.command(c, "EVAL", redisTokenBucketScript, "1", "gatego:rl:"+key, strconv.FormatFloat(rate, 'f', -1, 64), strconv.Itoa(burst), strconv.FormatInt(time.Now().UnixMilli(), 10))
	r.put(c, err == nil)
	if err != nil {
		return false, err
	}
	n, ok := v.(int64)
	if !ok {
		return false, fmt.Errorf("unexpected redis reply %T", v)
	}
	return n == 1, nil
}
func (r *RedisLimiter) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.mu.Unlock()
	for {
		select {
		case c := <-r.pool:
			if c != nil {
				_ = c.Close()
			}
		default:
			return nil
		}
	}
}
func (r *RedisLimiter) command(c *redisConn, args ...string) (any, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := c.Write([]byte(b.String())); err != nil {
		return nil, err
	}
	return readRESP(c.r)
}
func readRESP(r *bufio.Reader) (any, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if len(line) < 3 {
		return nil, errors.New("short redis reply")
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	switch line[0] {
	case '+':
		return line[1:], nil
	case '-':
		return nil, errors.New(line[1:])
	case ':':
		return strconv.ParseInt(line[1:], 10, 64)
	case '$':
		n, err := strconv.Atoi(line[1:])
		if err != nil {
			return nil, err
		}
		if n < 0 {
			return nil, nil
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		return string(buf[:n]), nil
	default:
		return nil, fmt.Errorf("unsupported redis reply %q", line)
	}
}
