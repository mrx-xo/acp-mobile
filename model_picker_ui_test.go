package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestModelPickerSearchAppearsOnlyOnOverflowAndFilters(t *testing.T) {
	page := newChatTestPage(t)
	page.eval(t, `
  document.getElementById('sp-close').click(); currentBufferName='model-search-chat';
  window.__modelCount=2; window.__modelWrites=[];
  const realFetch=window.fetch;
  window.fetch=async(url,options)=>{
    if(url.endsWith('/api/models')) return {ok:true,json:async()=>({current:'vendor/model-0',models:
      Array.from({length:window.__modelCount},(_,i)=>({id:'vendor/model-'+i,name:'Model '+i,description:'Model description'}))})};
    if(url.endsWith('/api/model')) {window.__modelWrites.push(JSON.parse(options.body));return {ok:true,json:async()=>({ok:true,current:'vendor/model-37'})};}
    return realFetch(url,options);
  };
  openModelPicker();`)
	page.waitFor(t, `document.querySelectorAll('#md-list .md-row').length === 2`)
	if hidden := page.eval(t, `document.getElementById('md-search')?.hidden === true`); hidden != true {
		t.Fatal("a short model list should keep search hidden")
	}
	page.eval(t, `window.__modelCount=6;openModelPicker()`)
	page.waitFor(t, `document.querySelectorAll('#md-list .md-row').length === 6`)
	if hidden := page.eval(t, `document.getElementById('md-search').hidden`); hidden != true {
		t.Fatal("six models should fit the tall viewport without search")
	}
	page.call(t, "Emulation.setDeviceMetricsOverride", map[string]interface{}{"width": 320, "height": 350, "deviceScaleFactor": 1, "mobile": true})
	page.waitFor(t, `!document.getElementById('md-search').hidden`)
	page.call(t, "Emulation.setDeviceMetricsOverride", map[string]interface{}{"width": 393, "height": 852, "deviceScaleFactor": 1, "mobile": true})
	page.eval(t, `window.__modelCount=80;openModelPicker()`)
	page.waitFor(t, `document.querySelectorAll('#md-list .md-row').length === 80 && !document.getElementById('md-search').hidden`)
	if dir := os.Getenv("SYZYGY_UI_SHOTS"); dir != "" {
		var shot struct{ Data string }
		if err := json.Unmarshal(page.call(t, "Page.captureScreenshot", map[string]interface{}{"format": "png"}), &shot); err != nil {
			t.Fatal(err)
		}
		data, err := base64.StdEncoding.DecodeString(shot.Data)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "model-search.png"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	state := page.evalObject(t, `(()=>{
    const q=document.getElementById('md-search'),body=document.getElementById('md-body');
    const top=q.getBoundingClientRect().top; body.scrollTop=body.scrollHeight;
    const pinned=top===q.getBoundingClientRect().top;
    q.value='  VENDOR/model-37  ';q.dispatchEvent(new Event('input'));
    return {pinned,visible:!q.hidden,rows:[...document.querySelectorAll('#md-list .md-row')].map(r=>r.textContent),scroll:body.scrollTop};
  })()`)
	if state["pinned"] != true || state["visible"] != true || state["scroll"].(float64) != 0 {
		t.Fatalf("search must stay pinned and reset list scroll: %#v", state)
	}
	rows := state["rows"].([]interface{})
	if len(rows) != 1 || rows[0] != "Model 37Model description" {
		t.Fatalf("case-insensitive ID search: %#v", rows)
	}
	state = page.evalObject(t, `(()=>{
    const q=document.getElementById('md-search');q.value='does-not-exist';q.dispatchEvent(new Event('input'));
    const empty=document.getElementById('md-list').textContent;
    q.value='';q.dispatchEvent(new Event('input'));
    return {empty,count:document.querySelectorAll('#md-list .md-row').length,current:document.querySelector('#md-list .sel')?.textContent,visible:!q.hidden};
  })()`)
	if state["empty"] != "No matching models." || state["count"].(float64) != 80 || state["current"] != "Model 0 (current)Model description" || state["visible"] != true {
		t.Fatalf("clearing search must restore the models and current selection: %#v", state)
	}
	// A smaller viewport must leave the input and results reachable together.
	page.call(t, "Emulation.setDeviceMetricsOverride", map[string]interface{}{"width": 320, "height": 430, "deviceScaleFactor": 1, "mobile": true})
	state = page.evalObject(t, `(()=>{
    const q=document.getElementById('md-search'),body=document.getElementById('md-body');
    q.focus();q.value='Model 37';q.dispatchEvent(new Event('input'));
    return {input:q.getBoundingClientRect().bottom,body:body.clientHeight,bottom:document.getElementById('model-sheet').getBoundingClientRect().bottom,width:document.documentElement.scrollWidth};
  })()`)
	if state["input"].(float64) > 430 || state["body"].(float64) <= 0 || state["bottom"].(float64) > 430 || state["width"].(float64) > 320 {
		t.Fatalf("small viewport layout: %#v", state)
	}
	page.call(t, "Emulation.setTouchEmulationEnabled", map[string]interface{}{"enabled": true, "maxTouchPoints": 1})
	point := page.evalObject(t, `(()=>{const r=document.querySelector('#md-list .md-row').getBoundingClientRect();return {x:r.left+r.width/2,y:r.top+r.height/2};})()`)
	page.call(t, "Input.dispatchTouchEvent", map[string]interface{}{"type": "touchStart", "touchPoints": []map[string]interface{}{{"x": point["x"], "y": point["y"]}}})
	page.call(t, "Input.dispatchTouchEvent", map[string]interface{}{"type": "touchEnd", "touchPoints": []interface{}{}})
	page.waitFor(t, `window.__modelWrites.length === 1 && !document.getElementById('model-sheet').classList.contains('visible')`)
	state = page.evalObject(t, `window.__modelWrites[0]`)
	if state["bufferName"] != "model-search-chat" || state["modelId"] != "vendor/model-37" {
		t.Fatalf("filtered choice sent wrong model: %#v", state)
	}
	page.eval(t, `window.__modelCount=2;openModelPicker()`)
	page.waitFor(t, `document.querySelectorAll('#md-list .md-row').length === 2`)
	state = page.evalObject(t, `({hidden:document.getElementById('md-search').hidden,query:document.getElementById('md-search').value})`)
	if state["hidden"] != true || state["query"] != "" {
		t.Fatalf("reopening must reset search and recompute overflow: %#v", state)
	}
}
