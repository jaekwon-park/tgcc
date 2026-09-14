package router

import (
	"context"
	"fmt"
	"strings"

	"github.com/jaekwon-park/tgcc/internal/bot"
	"github.com/jaekwon-park/tgcc/internal/store"
)

func (r *Router) handleCodexMessage(ctx context.Context, update bot.Update, topic *store.Topic) error {
	sess, err := r.mgr.GetSessionByTopic(topic.ID)
	if err != nil {
		return err
	}
	if sess == nil || sess.CLI != topic.CLI {
		if topic.WorkspacePath == "" {
			r.sender.Enqueue(bot.OutgoingMsg{ChatID: topic.ChatID, ThreadID: topic.ThreadID, Text: "/new <workspace>로 시작하세요."})
			return nil
		}
		sess, err = r.mgr.Spawn(ctx, topic.ID, topic.WorkspacePath, topic.ChatID, topic.ThreadID, topic.ClaudeModel.String)
		if err != nil {
			r.sender.Enqueue(bot.OutgoingMsg{ChatID: topic.ChatID, ThreadID: topic.ThreadID, Text: "❌ Codex 시작 실패: " + err.Error()})
			return err
		}
	}
	text := strings.TrimSpace(update.Message.Text)
	if text == "" {
		text = strings.TrimSpace(update.Message.Caption)
	}
	var images []string
	if len(update.Message.Photo) > 0 {
		photo := update.Message.Photo[len(update.Message.Photo)-1]
		path, err := r.downloadTelegramFile(ctx, photo.FileID, sess.WorkspacePath)
		if err != nil {
			r.sender.Enqueue(bot.OutgoingMsg{ChatID: topic.ChatID, ThreadID: topic.ThreadID, Text: "❌ 이미지 다운로드 실패"})
			return err
		}
		images = append(images, path)
		if text == "" {
			text = "첨부한 이미지를 확인해 주세요."
		}
	}
	if text == "" {
		return nil
	}
	if err := r.mgr.ForwardImages(ctx, sess.ID, text, images); err != nil {
		r.sender.Enqueue(bot.OutgoingMsg{ChatID: topic.ChatID, ThreadID: topic.ThreadID, Text: fmt.Sprintf("⚠️ 메시지 접수 실패: %v", err)})
		return err
	}
	return nil
}
