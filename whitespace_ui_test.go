package main

import "testing"

func TestPromptWhitespaceSurvivesSendQueueSelectionAndCopy(t *testing.T) {
	page := openComposerTestPage(t, 844, 844)
	state := page.evalObject(t, `(async () => {
		showChat();
		sessionId = 'whitespace';
		const sent = [];
		ws = {readyState: 1, send: raw => sent.push(JSON.parse(raw))};
		const original = '\n  first  line\n\n\n\tsecond line  \n';
		const selectedText = element => {
			const range = document.createRange();
			range.selectNodeContents(element);
			const selection = getSelection();
			selection.removeAllRanges(); selection.addRange(range);
			const text = selection.toString(); selection.removeAllRanges();
			return text;
		};
		promptEl.value = original;
		sendPrompt();
		const message = messagesEl.querySelector('.msg.user');
		const result = {
			payload: sent[0].params.prompt[0].text,
			selected: selectedText(message.querySelector('.text'))
		};
		openReader(message);
		result.reader = selectedText(document.querySelector('#reader-body .text'));
		closeReader();
		let copied;
		Object.defineProperty(navigator, 'clipboard', {configurable: true,
			value: {writeText: async text => { copied = text; }}});
		openMsgMenu(message);
		document.getElementById('mm-copy').click();
		result.copied = copied;
		// Pasting the copied plain text into the composer while busy queues it.
		promptEl.value = copied;
		sendPrompt();
		result.queued = messageQueue[0].text;
		flushMessageQueue();
		result.queuePayload = sent[1].params.prompt[0].text;
		const replayed = addUserMsg(original, 'sent');
		result.replayed = selectedText(replayed.querySelector('.text'));
		setProcessing(false);
		promptEl.value = ' \t\n';
		sendPrompt();
		result.emptySent = sent.length !== 2;
		return result;
	})()`)
	want := "\n  first  line\n\n\n\tsecond line  \n"
	for _, key := range []string{"payload", "copied", "queued", "queuePayload"} {
		if state[key] != want {
			t.Errorf("%s = %q, want exact whitespace %q", key, state[key], want)
		}
	}
	// Browser selection omits a final newline at the block boundary. The
	// whole-message Copy text action above must still preserve it exactly.
	for _, key := range []string{"selected", "reader", "replayed"} {
		if want := "\n  first  line\n\n\n\tsecond line  "; state[key] != want {
			t.Errorf("%s = %q, want %q", key, state[key], want)
		}
	}
	if state["emptySent"] != false {
		t.Error("whitespace-only message must not be sent")
	}
}

func TestAgentWhitespaceSurvivesSelectionAndCopy(t *testing.T) {
	page := openComposerTestPage(t, 844, 844)
	state := page.evalObject(t, `(async () => {
		showChat();
		const original = 'first  line\n\tindented\n\n\nlast';
		const message = addAgentMsg(original);
		const range = document.createRange();
		range.selectNodeContents(message.querySelector('.md'));
		getSelection().removeAllRanges(); getSelection().addRange(range);
		const selected = getSelection().toString();
		getSelection().removeAllRanges();
		let copied;
		Object.defineProperty(navigator, 'clipboard', {configurable: true,
			value: {writeText: async text => { copied = text; }}});
		openMsgMenu(message);
		document.getElementById('mm-copy').click();
		const whole = copied;
		const code = addAgentMsg('\x60\x60\x60text\n  a  b\n\n\tend  \n\n\x60\x60\x60');
		code.querySelector('.code-copy').click();
		return {selected, whole, code: copied};
	})()`)
	for _, key := range []string{"selected", "whole"} {
		if want := "first  line\n\tindented\n\n\nlast"; state[key] != want {
			t.Errorf("%s = %q, want %q", key, state[key], want)
		}
	}
	if want := "  a  b\n\n\tend  \n\n"; state["code"] != want {
		t.Errorf("copied code = %q, want %q", state["code"], want)
	}
}
