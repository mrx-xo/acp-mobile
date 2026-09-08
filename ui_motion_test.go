package main

import "testing"

// Appending a live chunk must not replace settled text or restart its fade.
// Historical messages stay still, including when a resumed reply continues.
func TestSoftStreamingPreservesWordsAndMarkdown(t *testing.T) {
	page := openComposerTestPage(t, 844, 844)
	page.call(t, "Emulation.setEmulatedMedia", map[string]interface{}{
		"features": []map[string]string{{"name": "prefers-reduced-motion", "value": "no-preference"}},
	})
	state := page.evalObject(t, `(async () => {
		showChat();
		const reply = addAgentMsg('');
		updateAgentMsg(reply, 'First words');
		const first = reply.querySelector('[data-stream-word]');
		const animation = first && first.getAnimations()[0];
		await new Promise(r => setTimeout(r, 40));
		updateAgentMsg(reply, 'First words arrive **softly**.');
		const preserved = first === reply.querySelector('[data-stream-word]');
		const sameAnimation = !!first && first.getAnimations()[0] === animation;
		const text = reply.querySelector('.md').textContent;
		const resumed = addAgentMsg('Earlier words');
		const historyAnimated = resumed.getAnimations({subtree: true}).length > 0;
		updateAgentMsg(resumed, 'Earlier words plus new words');
		const old = resumed.querySelector('[data-stream-word]');
		const oldAnimated = old && old.getAnimations().length > 0;
		const newAnimated = [...resumed.querySelectorAll('[data-stream-word]')]
			.some(w => w.textContent === 'new' && w.getAnimations().length > 0);
		// Completing Markdown syntax must still produce the canonical renderer
		// output, including code and the copy-button attributes.
		const markdown = 'First words arrive **softly**.\n\n[link](https://example.com)\n\n' +
			'\x60\x60\x60js\nconst n = 1;\n\x60\x60\x60\n\n1. item';
		updateAgentMsg(reply, markdown);
		const copy = reply.querySelector('.md').cloneNode(true);
		copy.querySelectorAll('[data-stream-word]').forEach(w => w.replaceWith(document.createTextNode(w.textContent)));
		const expected = document.createElement('div');
		expected.innerHTML = renderAgentMarkdown(markdown);
		const canonical = copy.innerHTML === expected.innerHTML;
		return {hasFade: !!animation, preserved, sameAnimation, text, historyAnimated,
			oldAnimated, newAnimated, canonical,
			codeAnimated: !!reply.querySelector('pre [data-stream-word], code [data-stream-word]')};
	})()`)
	if state["hasFade"] != true || state["preserved"] != true || state["sameAnimation"] != true {
		t.Fatalf("live chunks must animate new words while preserving existing word nodes and fades: %v", state)
	}
	if state["text"] != "First words arrive softly." || state["canonical"] != true || state["codeAnimated"] != false {
		t.Fatalf("stream motion must preserve Markdown and leave code alone: %v", state)
	}
	if state["historyAnimated"] != false || state["oldAnimated"] != false || state["newAnimated"] != true {
		t.Fatalf("only new live text should animate when a historical reply resumes: %v", state)
	}

	page.call(t, "Emulation.setEmulatedMedia", map[string]interface{}{
		"features": []map[string]string{{"name": "prefers-reduced-motion", "value": "reduce"}},
	})
	state = page.evalObject(t, `(() => {
		const reply = addAgentMsg('');
		updateAgentMsg(reply, 'Available immediately');
		return {text: reply.querySelector('.md').textContent, animations: reply.getAnimations({subtree:true}).length};
	})()`)
	if state["text"] != "Available immediately" || state["animations"] != float64(0) {
		t.Fatalf("reduced motion must show the whole reply immediately: %v", state)
	}
}

// Completing link syntax changes element structure, not the age of its label.
func TestSoftStreamingDoesNotRefadeCompletedLinkLabels(t *testing.T) {
	page := openComposerTestPage(t, 844, 844)
	page.call(t, "Emulation.setEmulatedMedia", map[string]interface{}{
		"features": []map[string]string{{"name": "prefers-reduced-motion", "value": "no-preference"}},
	})
	state := page.evalObject(t, `(async () => {
  const reply = addAgentMsg('');
  updateAgentMsg(reply, 'Read [these settled words]');
  await new Promise(r => setTimeout(r, 400));
  updateAgentMsg(reply, 'Read [these settled words](https://example.com)');
  const link = reply.querySelector('a');
  return {label: link && link.textContent, animated: link && link.getAnimations({subtree: true}).length};
 })()`)
	if state["label"] != "these settled words" || state["animated"] != float64(0) {
		t.Fatalf("finishing a Markdown link must not fade its existing label again: %v", state)
	}
}

func TestSoftBackSwipeSettlesBeforeNavigatingAndCancels(t *testing.T) {
	page := openComposerTestPage(t, 844, 844)
	page.call(t, "Emulation.setEmulatedMedia", map[string]interface{}{
		"features": []map[string]string{{"name": "prefers-reduced-motion", "value": "no-preference"}},
	})
	state := page.evalObject(t, `(async () => {
		showChat();
		const touch = (type, x) => {
			const t = new Touch({identifier: 1, target: chatViewEl, clientX: x, clientY: 200});
			chatViewEl.dispatchEvent(new TouchEvent(type, {touches: type === 'touchstart' || type === 'touchmove' ? [t] : [], changedTouches:[t], bubbles:true}));
		};
		touch('touchstart', 5); touch('touchmove', 130);
		const followsFinger = chatViewEl.style.transform === 'translateX(125px)';
		touch('touchcancel', 130);
		await new Promise(r => setTimeout(r, 450));
		const cancelled = chatViewEl.classList.contains('visible') && !chatViewEl.style.transform;
		touch('touchstart', 5); touch('touchmove', 130); touch('touchend', 130);
		const visibleDuringExit = chatViewEl.classList.contains('visible');
		await new Promise(r => setTimeout(r, 500));
		return {followsFinger, cancelled, visibleDuringExit,
			returned: !chatViewEl.classList.contains('visible') && !orreryEl.classList.contains('hidden'),
			cleanTransform: !chatViewEl.style.transform};
	})()`)
	if state["followsFinger"] != true || state["cancelled"] != true || state["visibleDuringExit"] != true ||
		state["returned"] != true || state["cleanTransform"] != true {
		t.Fatalf("swipe must track the finger, cancel cleanly, and finish its exit before navigating: %v", state)
	}
}
