package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"time"

	"math/rand"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	_ "github.com/lib/pq"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
)

const name = "nostr-mahjongbot"

const version = "0.0.24"

var revision = "HEAD"

const help = `
This is a small Mahjong game. The game is played by mentions to me. I deal Mahjong tiles and you specify the tiles with the "drop" command to discard. If you want to judge, execute the "check" command.

start
  Start the game.
drop NUM
  Drop the NUM tile.
check
  Judge the tiles.
`

var (
	cmdHelp  = regexp.MustCompile(`\bhelp$`)
	cmdStart = regexp.MustCompile(`\bstart$`)
	cmdDrop  = regexp.MustCompile(`\bdrop ([0-9]+)$`)
	cmdCheck = regexp.MustCompile(`\bcheck$`)

	//go:embed static
	assets embed.FS
)

func judge2(ctx Result) bool {
	if len(ctx.mentu) == 12 {
		return true
	}
	for n := 0; n < len(ctx.hai); n++ {
		if ctx.hai[n] >= 3 {
			ctx.mentu = append(ctx.mentu, n, n, n)
			ctx.hai[n] -= 3
			if judge2(ctx) {
				return true
			}
		}
	}
	for n := 0; n < len(ctx.hai)-2; n++ {
		if ctx.hai[n] > 0 && ctx.hai[n+1] > 0 && ctx.hai[n+2] > 0 {
			ctx.mentu = append(ctx.mentu, n+0, n+1, n+2)
			ctx.hai[n+0] -= 1
			ctx.hai[n+1] -= 1
			ctx.hai[n+2] -= 1
			if judge2(ctx) {
				return true
			}
		}
	}
	return false
}

type Result struct {
	hai   []int
	mentu []int
	atama int
}

func judge1(hai []int) Result {
	ctx := Result{
		hai:   hai,
		mentu: []int{},
		atama: -1,
	}
	for n := 0; n < len(hai); n++ {
		if hai[n] >= 2 {
			nhai := make([]int, len(hai))
			copy(nhai, hai)
			ctx = Result{hai: nhai, mentu: []int{}, atama: n}
			ctx.hai[n] -= 2
			if judge2(ctx) {
				break
			}
		}
		ctx.atama = -1
	}
	return ctx
}

func (g *game) judge() *Result {
	thai := []int{0, 0, 0, 0, 0, 0, 0, 0, 0}
	for n := 0; n < len(g.Data.Hai); n++ {
		thai[g.Data.Hai[n]] += 1
	}
	ret := judge1(thai)
	if ret.atama == -1 {
		return nil
	}
	return &ret
}

func (g *game) drop(v int) bool {
	for i := 0; i < len(g.Data.Hai); i++ {
		if g.Data.Hai[i] == v {
			copy(g.Data.Hai[i:], g.Data.Hai[i+1:])
			g.Data.Hai = g.Data.Hai[:len(g.Data.Hai)-1]
			return true
		}
	}
	return false
}

func (g *game) take() int {
	rest := 0
	for _, v := range g.Data.Mountain {
		rest += v
	}
	if rest == 0 {
		return -1
	}
	for {
		n := rand.Int() % 9
		if g.Data.Mountain[n] > 0 {
			g.Data.Mountain[n] -= 1
			g.Data.Hai = append(g.Data.Hai, n)
			sort.Ints(g.Data.Hai)
			return n
		}
	}
}

type Data struct {
	Mountain []int `json:"mountain"`
	Hai      []int `json:"hai"`
}

type game struct {
	bun.BaseModel `bun:"table:mahjong_game,alias:g"`
	ID            string    `bun:"id,pk,notnull" json:"id"`
	Npub          string    `bun:"npub,notnull" json:"npub"`
	Data          Data      `bun:"data,type:jsonb" json:"data"`
	Count         int       `bun:"count,notnull" json:"count"`
	CreatedAt     time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
}

func upload(buf *bytes.Buffer) (string, error) {
	req, err := http.NewRequest(http.MethodPost, "https://void.cat/upload?cli=true", buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("V-Content-Type", "image/png")
	result := sha256.Sum256(buf.Bytes())
	req.Header.Set("V-Full-Digest", hex.EncodeToString(result[:]))
	req.Header.Set("V-Filename", "image.png")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer req.Body.Close()

	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(b) + ".png", nil
}

func makeImage(hai []int) (string, error) {
	bounds := image.Rect(0, 0, (128+2)*14+3+4, 183+4)

	dst := image.NewRGBA(bounds)

	for i, v := range hai {
		f, err := assets.Open(fmt.Sprintf("static/mahjong-p%d.png", v+1))
		if err != nil {
			return "", err
		}
		defer f.Close()
		emoji, _, err := image.Decode(f)
		if err != nil {
			return "", err
		}
		off := 0
		if i == 13 {
			off = 2
		}
		draw.Draw(
			dst,
			image.Rect(
				i*(emoji.Bounds().Dx()+2)+off+2,
				2,
				(i+1)*(emoji.Bounds().Dx()+2)+off,
				emoji.Bounds().Dy(),
			),
			emoji, image.ZP,
			draw.Over)
	}

	var buf bytes.Buffer
	err := png.Encode(&buf, dst)
	if err != nil {
		return "", err
	}
	return upload(&buf)
}

func main() {
	db, err := sql.Open("postgres", os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Println(err)
		return
	}

	bundb := bun.NewDB(db, pgdialect.New())
	defer bundb.Close()

	_, err = bundb.NewCreateTable().Model((*game)(nil)).IfNotExists().Exec(context.Background())
	if err != nil {
		log.Fatal(err)
		return
	}

	var sk, pub string
	if _, s, err := nip19.Decode(os.Getenv("BOT_NSEC")); err != nil {
		log.Fatal(err)
	} else {
		sk = s.(string)
	}
	if pub, err = nostr.GetPublicKey(sk); err == nil {
		if _, err := nip19.EncodePublicKey(pub); err != nil {
			log.Fatal(err)
		}
	} else {
		log.Fatal(err)
	}

	enc := json.NewEncoder(os.Stdout)

	e := echo.New()
	e.Use(middleware.Logger())
	e.POST("/api", func(c echo.Context) error {
		ev := nostr.Event{}
		err := json.NewDecoder(c.Request().Body).Decode(&ev)
		if err != nil {
			log.Println(err)
			return c.JSON(http.StatusInternalServerError, err.Error())
		}
		enc.Encode(ev)

		var g game
		var etag *nostr.Tag
		for i := 0; i < len(ev.Tags); i++ {
			e := ev.Tags[i]
			if e.Key() == "e" && len(e) > 2 && e[2] == "reply" {
				etag = &e
			}
		}
		if etag == nil {
			etag = ev.Tags.GetFirst([]string{"e"})
		}

		if cmdHelp.MatchString(ev.Content) {
			from := ev.PubKey

			ev.PubKey = pub
			ev.Tags = nostr.Tags{}
			ev.Tags = ev.Tags.AppendUnique(nostr.Tag{"e", ev.ID})
			ev.Tags = ev.Tags.AppendUnique(nostr.Tag{"p", from})
			ev.CreatedAt = nostr.Now()
			ev.Kind = nostr.KindTextNote

			ev.Content = help
			if err := ev.Sign(sk); err != nil {
				log.Println(err)
				log.Println("fail sign", err)
				return c.JSON(http.StatusInternalServerError, err.Error())
			}
			return c.JSON(http.StatusOK, ev)
		} else if etag == nil && cmdStart.MatchString(ev.Content) {
			from := ev.PubKey

			ev.PubKey = pub
			ev.Tags = nostr.Tags{}
			ev.Tags = ev.Tags.AppendUnique(nostr.Tag{"e", ev.ID})
			ev.Tags = ev.Tags.AppendUnique(nostr.Tag{"p", from})
			ev.CreatedAt = nostr.Now()
			ev.Kind = nostr.KindTextNote

			g.Npub = from
			g.Data.Mountain = []int{4, 4, 4, 4, 4, 4, 4, 4, 4}
			for n := 0; n < 14; n++ {
				g.take()
			}
			g.Count++
			img, err := makeImage(g.Data.Hai)
			if err != nil {
				log.Println("fail makeImage", err)
				return c.JSON(http.StatusInternalServerError, err.Error())
			}
			ev.Content = img
			if err := ev.Sign(sk); err != nil {
				log.Println(err)
				log.Println("fail sign", err)
				return c.JSON(http.StatusInternalServerError, err.Error())
			}

			g.ID = ev.ID
			_, err = bundb.NewInsert().Model(&g).Exec(context.Background())
			if err != nil {
				log.Println("fail insert", err)
				return c.JSON(http.StatusInternalServerError, err.Error())
			}

			return c.JSON(http.StatusOK, ev)
		} else if etag != nil && cmdDrop.MatchString(ev.Content) {
			from := ev.PubKey

			ev.PubKey = pub
			ev.Tags = nostr.Tags{}
			ev.Tags = ev.Tags.AppendUnique(nostr.Tag{"e", ev.ID})
			ev.Tags = ev.Tags.AppendUnique(nostr.Tag{"p", from})
			ev.CreatedAt = nostr.Now()
			ev.Kind = nostr.KindTextNote

			//if from != "2c7cc62a697ea3a7826521f3fd34f0cb273693cbe5e9310f35449f43622a5cdc" {
			//	ev.Content = "Still under development"
			//	if err := ev.Sign(sk); err != nil {
			//		log.Println(err)
			//		return c.JSON(http.StatusInternalServerError, err.Error())
			//	}
			//	return c.JSON(http.StatusOK, ev)
			//}

			matched := cmdDrop.FindStringSubmatch(ev.Content)
			if len(matched) != 2 {
				ev.Content = "Invalid command"
				if err := ev.Sign(sk); err != nil {
					log.Println(err)
					return c.JSON(http.StatusInternalServerError, err.Error())
				}
				return c.JSON(http.StatusOK, ev)
			}
			v, err := strconv.Atoi(matched[1])
			if v < 1 || v > 9 {
				ev.Content = "Invalid number"
				if err := ev.Sign(sk); err != nil {
					log.Println(err)
					return c.JSON(http.StatusInternalServerError, err.Error())
				}
				return c.JSON(http.StatusOK, ev)
			}

			err = bundb.NewSelect().Model((*game)(nil)).Where("ID = ?", etag.Value()).Scan(context.Background(), &g)
			if err != nil {
				ev.Content = "Invalid reference"
				if err := ev.Sign(sk); err != nil {
					log.Println(err)
					return c.JSON(http.StatusInternalServerError, err.Error())
				}
				return c.JSON(http.StatusOK, ev)
			}
			if g.Npub != from {
				ev.Content = "You are not game owner"
				if err := ev.Sign(sk); err != nil {
					log.Println(err)
					return c.JSON(http.StatusInternalServerError, err.Error())
				}
				return c.JSON(http.StatusOK, ev)
			}

			if !g.drop(v - 1) {
				ev.Content = "No such tiles"
				if err := ev.Sign(sk); err != nil {
					log.Println(err)
					return c.JSON(http.StatusInternalServerError, err.Error())
				}
				return c.JSON(http.StatusOK, ev)
			}
			if g.take() == -1 {
				ev.Content = "No more tiles"
				if err := ev.Sign(sk); err != nil {
					log.Println(err)
					return c.JSON(http.StatusInternalServerError, err.Error())
				}
			}
			g.Count++

			img, err := makeImage(g.Data.Hai)
			if err != nil {
				log.Println(err)
				return c.JSON(http.StatusInternalServerError, err.Error())
			}
			ev.Content = img
			if err := ev.Sign(sk); err != nil {
				log.Println(err)
				return c.JSON(http.StatusInternalServerError, err.Error())
			}

			tx, err := bundb.Begin()
			if err != nil {
				log.Println(err)
				return c.JSON(http.StatusInternalServerError, err.Error())
			}
			defer tx.Rollback()

			_, err = tx.NewDelete().Model((*game)(nil)).Where("ID = ?", etag.Value()).Exec(context.Background())
			if err != nil {
				log.Println(err)
				return c.JSON(http.StatusInternalServerError, err.Error())
			}
			g.ID = ev.ID
			_, err = tx.NewInsert().Model(&g).Exec(context.Background())
			if err != nil {
				log.Println(err)
				return c.JSON(http.StatusInternalServerError, err.Error())
			}
			tx.Commit()

			return c.JSON(http.StatusOK, ev)
		} else if cmdCheck.MatchString(ev.Content) {
			from := ev.PubKey

			ev.PubKey = pub
			ev.Tags = nostr.Tags{}
			ev.Tags = ev.Tags.AppendUnique(nostr.Tag{"e", ev.ID})
			ev.Tags = ev.Tags.AppendUnique(nostr.Tag{"p", from})
			ev.CreatedAt = nostr.Now()
			ev.Kind = nostr.KindTextNote

			//if from != "2c7cc62a697ea3a7826521f3fd34f0cb273693cbe5e9310f35449f43622a5cdc" {
			//	ev.Content = "Still under development"
			//	if err := ev.Sign(sk); err != nil {
			//		log.Println(err)
			//		return c.JSON(http.StatusInternalServerError, err.Error())
			//	}
			//	return c.JSON(http.StatusOK, ev)
			//}

			err = bundb.NewSelect().Model((*game)(nil)).Where("ID = ?", etag.Value()).Scan(context.Background(), &g)
			if err != nil {
				ev.Content = "Invalid reference"
				if err := ev.Sign(sk); err != nil {
					log.Println(err)
					return c.JSON(http.StatusInternalServerError, err.Error())
				}
				return c.JSON(http.StatusOK, ev)
			}
			if g.Npub != from {
				ev.Content = "You are not game owner"
				if err := ev.Sign(sk); err != nil {
					log.Println(err)
					return c.JSON(http.StatusInternalServerError, err.Error())
				}
				return c.JSON(http.StatusOK, ev)
			}

			tx, err := bundb.Begin()
			if err != nil {
				log.Println(err)
				return c.JSON(http.StatusInternalServerError, err.Error())
			}
			defer tx.Rollback()

			_, err = tx.NewDelete().Model((*game)(nil)).Where("ID = ?", etag.Value()).Exec(context.Background())
			if err != nil {
				log.Println(err)
				return c.JSON(http.StatusInternalServerError, err.Error())
			}
			tx.Commit()

			result := g.judge()
			if result == nil {
				ev.Content = "Misjudge, game over"
				if err := ev.Sign(sk); err != nil {
					log.Println(err)
					return c.JSON(http.StatusInternalServerError, err.Error())
				}
				return c.JSON(http.StatusOK, ev)
			}

			if g.Count == 1 {
				ev.Content = fmt.Sprint("Win, game over (天和)")
			} else {
				ev.Content = fmt.Sprintf("Win, game over (count: %d)", g.Count)
			}
			if err := ev.Sign(sk); err != nil {
				log.Println(err)
				return c.JSON(http.StatusInternalServerError, err.Error())
			}
			return c.JSON(http.StatusOK, ev)
		}

		return c.JSON(http.StatusOK, "")
	})
	e.Logger.Fatal(e.Start(":8989"))
}
