package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/fatih/color"
	"github.com/go-faster/jx"
	"github.com/gotd/contrib/middleware/ratelimit"
	"github.com/gotd/td/telegram/peers"
	"github.com/gotd/td/telegram/query"
	"github.com/gotd/td/telegram/query/channels/participants"
	"github.com/gotd/td/telegram/query/messages"
	"github.com/gotd/td/tg"
	"github.com/jedib0t/go-pretty/v6/progress"
	"go.uber.org/multierr"
	"golang.org/x/time/rate"

	"github.com/iyear/tdl/app/internal/tgc"
	"github.com/iyear/tdl/pkg/prog"
	"github.com/iyear/tdl/pkg/storage"
	"github.com/iyear/tdl/pkg/texpr"
	"github.com/iyear/tdl/pkg/tmedia"
	"github.com/iyear/tdl/pkg/utils"
)

const (
	rateInterval = 550 * time.Millisecond
	rateBucket   = 2
)

type ExportOptions struct {
	Type        string
	Chat        string
	Thread      int // topic id in forum, message id in group
	Input       []int
	Output      string
	Filter      string
	OnlyMedia   bool
	WithContent bool
	Raw         bool
	All         bool
	Users       bool
}

type Message struct {
	ID   int    `json:"id"`
	Type string `json:"type"`
	File string `json:"file,omitempty"`
	Date int    `json:"date,omitempty"`
	Text string `json:"text,omitempty"`
	User string `json:"user,omitempty"`
}

const (
	ExportTypeTime string = "time"
	ExportTypeID   string = "id"
	ExportTypeLast string = "last"
)

func Export(ctx context.Context, opts *ExportOptions) error {
	c, kvd, err := tgc.NoLogin(ctx, ratelimit.New(rate.Every(rateInterval), rateBucket))
	if err != nil {
		return err
	}

	// only output available fields
	if opts.Filter == "-" {
		fg := texpr.NewFieldsGetter(nil)

		fields, err := fg.Walk(&message{})
		if err != nil {
			return fmt.Errorf("failed to walk fields: %w", err)
		}

		fmt.Print(fg.Sprint(fields, true))
		return nil
	}

	filter, err := texpr.Compile(opts.Filter)
	if err != nil {
		return fmt.Errorf("failed to compile filter: %w", err)
	}

	return tgc.RunWithAuth(ctx, c, func(ctx context.Context) (rerr error) {
		var peer peers.Peer

		manager := peers.Options{Storage: storage.NewPeers(kvd)}.Build(c.API())
		if opts.Chat == "" { // defaults to me(saved messages)
			peer, err = manager.Self(ctx)
		} else {
			peer, err = utils.Telegram.GetInputPeer(ctx, manager, opts.Chat)
		}
		if err != nil {
			return fmt.Errorf("failed to get peer: %w", err)
		}

		color.Yellow("WARN: Export only generates minimal JSON for tdl download, not for backup.")
		color.Cyan("Occasional suspensions are due to Telegram rate limitations, please wait a moment.")
		fmt.Println()

		color.Blue("Type: %s | Input: %v", opts.Type, opts.Input)

		pw := prog.New(progress.FormatNumber)
		pw.SetUpdateFrequency(200 * time.Millisecond)
		pw.Style().Visibility.TrackerOverall = false
		pw.Style().Visibility.ETA = false
		pw.Style().Visibility.Percentage = false

		tracker := prog.AppendTracker(pw, progress.FormatNumber, fmt.Sprintf("%s-%d", peer.VisibleName(), peer.ID()), 0)

		go pw.Render()

		usersList := map[int64]*tg.User{}

		if opts.Users {
			ch, ok := peer.(peers.Channel)
			if ok {
				iter := participants.NewIterator(query.NewQuery(c.API()).GetParticipants(ch.InputChannel()), 100)
				for iter.Next(ctx) {
					el := iter.Value()
					us, ok := el.User()
					if !ok {
						continue
					}

					usersList[us.ID] = us
				}
			}
		}

		var q messages.Query

		switch {
		case opts.Thread != 0: // topic messages, reply messages
			q = query.NewQuery(c.API()).Messages().GetReplies(peer.InputPeer()).MsgID(opts.Thread)
		default: // history
			q = query.NewQuery(c.API()).Messages().GetHistory(peer.InputPeer())
		}
		iter := messages.NewIterator(q, 100)

		switch opts.Type {
		case ExportTypeTime:
			iter = iter.OffsetDate(opts.Input[1] + 1)
		case ExportTypeID:
			iter = iter.OffsetID(opts.Input[1] + 1) // #89: retain the last msg id
		case ExportTypeLast:
		}

		f, err := os.Create(opts.Output)
		if err != nil {
			return err
		}
		defer multierr.AppendInvoke(&rerr, multierr.Close(f))

		enc := jx.NewStreamingEncoder(f, 512)
		defer multierr.AppendInvoke(&rerr, multierr.Close(enc))

		// process thread is reply type and peer is broadcast channel,
		// so we need to set discussion group id instead of broadcast id
		id := peer.ID()
		if p, ok := peer.(peers.Channel); opts.Thread != 0 && ok && p.IsBroadcast() {
			bc, _ := p.ToBroadcast()
			raw, err := bc.FullRaw(ctx)
			if err != nil {
				return fmt.Errorf("failed to get broadcast full raw: %w", err)
			}

			if id, ok = raw.GetLinkedChatID(); !ok {
				return fmt.Errorf("no linked group")
			}
		}

		enc.ObjStart()
		defer enc.ObjEnd()
		enc.Field("id", func(e *jx.Encoder) { e.Int64(id) })

		enc.FieldStart("messages")
		enc.ArrStart()
		defer enc.ArrEnd()

		count := int64(0)

	loop:
		for iter.Next(ctx) {
			msg := iter.Value()
			switch opts.Type {
			case ExportTypeTime:
				if msg.Msg.GetDate() < opts.Input[0] {
					break loop
				}
			case ExportTypeID:
				if msg.Msg.GetID() < opts.Input[0] {
					break loop
				}
			case ExportTypeLast:
				if count >= int64(opts.Input[0]) {
					break loop
				}
			}

			m, ok := msg.Msg.(*tg.Message)
			if !ok {
				continue
			}
			// only get media messages
			media, ok := tmedia.GetMedia(m)
			if !ok && !opts.All {
				continue
			}

			internalMsg := covertMessage(m)
			b, err := texpr.Run(filter, internalMsg)
			if err != nil {
				return fmt.Errorf("failed to run filter: %w", err)
			}
			if !b.(bool) { // filtered
				continue
			}

			var jsonMessage any
			if opts.Raw {
				jsonMessage = m
			} else {
				fileName := ""
				if media != nil { // #207
					fileName = media.Name
				}
				t := &Message{
					ID:   m.ID,
					Type: "message",
					File: fileName,
				}
				if opts.WithContent {
					t.Date = m.Date
					t.Text = m.Message
				}

				user, ok := usersList[internalMsg.FromID]
				if ok {
					if user.Bot {
						t.User = "Bot: " + user.Username
					}
					t.User = user.FirstName + " " + user.LastName + " (" + user.Username + ")"
				}
				jsonMessage = t
			}

			mb, err := json.Marshal(jsonMessage)
			if err != nil {
				return fmt.Errorf("failed to marshal message: %w", err)
			}
			enc.Raw(mb)

			count++
			tracker.SetValue(count)
		}

		if err = iter.Err(); err != nil {
			return err
		}

		tracker.MarkAsDone()
		prog.Wait(pw)
		return nil
	})
}
