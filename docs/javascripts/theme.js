/**
 * OVN-Kubernetes docs: Single-Wire, Go-struct, LocalStorage.
 * Day/Night emoji toggle (solar flare / lunar eclipse), Gemini AI Summarizer (side popup).
 */
(function () {
  "use strict";

  var STORAGE_KEY = "ovn-docs-theme";
  var SCHEME_BLUEPRINT = "default";
  var SCHEME_NERVE = "slate";

  /* Gemini API key: set window.OVN_GEMINI_API_KEY or data-gemini-api-key on the script tag. */

  function getStoredScheme() {
    try {
      return localStorage.getItem(STORAGE_KEY);
    } catch (e) {
      return null;
    }
  }

  function setStoredScheme(scheme) {
    try {
      localStorage.setItem(STORAGE_KEY, scheme);
    } catch (e) {}
  }

  function applyScheme(scheme) {
    var root = document.documentElement;
    root.setAttribute("data-md-color-scheme", scheme);
    var input = document.querySelector('input[id^="__palette"]');
    if (input && input.type === "checkbox") {
      var wantDark = scheme === SCHEME_NERVE;
      if (input.checked !== wantDark) {
        input.checked = wantDark;
        input.dispatchEvent(new Event("change", { bubbles: true }));
      }
    }
  }

  function initFromStorage() {
    var stored = getStoredScheme();
    if (stored === SCHEME_NERVE || stored === SCHEME_BLUEPRINT) {
      applyScheme(stored);
    }
  }

  /* ----- Day/Night emoji toggle: Sun (solar flare) / Moon (lunar eclipse) ----- */
  function createThemeEmojiBtn() {
    var existing = document.getElementById("ovn-theme-emoji-btn");
    if (existing) return existing;

    var btn = document.createElement("button");
    btn.id = "ovn-theme-emoji-btn";
    btn.type = "button";
    btn.className = "ovn-theme-emoji-btn";
    btn.setAttribute("aria-label", "Toggle theme (Blueprint / Nerve Center)");
    updateEmoji(btn);

    var label = document.querySelector('label[for^="__palette"]');
    if (label) label.style.setProperty("display", "none", "important");

    btn.addEventListener("click", function () {
      var current = document.documentElement.getAttribute("data-md-color-scheme") || SCHEME_BLUEPRINT;
      var next = current === SCHEME_NERVE ? SCHEME_BLUEPRINT : SCHEME_NERVE;

      var overlay = document.getElementById("ovn-theme-ripple");
      if (!overlay) {
        overlay = document.createElement("div");
        overlay.id = "ovn-theme-ripple";
        overlay.setAttribute("aria-hidden", "true");
        overlay.style.cssText = "position:fixed;inset:0;pointer-events:none;z-index:9999;opacity:0;transition:opacity 0.4s ease;display:flex;align-items:center;justify-content:center;font-size:4rem;";
        document.body.appendChild(overlay);
      }
      overlay.textContent = next === SCHEME_BLUEPRINT ? "\u2600\uFE0F" : "\uD83C\uDF19";
      overlay.style.background = next === SCHEME_NERVE ? "rgba(0,0,0,0.7)" : "rgba(255,255,255,0.85)";
      overlay.style.color = next === SCHEME_BLUEPRINT ? "#1a1a2e" : "#e8eef2";
      overlay.style.opacity = "0";
      overlay.offsetHeight;
      overlay.style.transition = "opacity 0.45s ease";
      overlay.style.opacity = "1";

      setTimeout(function () {
        applyScheme(next);
        setStoredScheme(next);
        updateEmoji(btn);
        overlay.style.opacity = "0";
        setTimeout(function () {
          overlay.textContent = "";
          overlay.style.transition = "";
        }, 450);
      }, 400);
    });

    var header = document.querySelector(".md-header__inner");
    if (header) header.appendChild(btn);
    return btn;
  }

  function updateEmoji(btn) {
    if (!btn) return;
    var scheme = document.documentElement.getAttribute("data-md-color-scheme") || SCHEME_BLUEPRINT;
    btn.textContent = scheme === SCHEME_NERVE ? "\uD83C\uDF19" : "\u2600\uFE0F";
  }

  function bindThemeToggle() {
    var input = document.querySelector('input[id^="__palette"]');
    if (input) {
      input.addEventListener("change", function () {
        setStoredScheme(input.checked ? SCHEME_NERVE : SCHEME_BLUEPRINT);
        var btn = document.getElementById("ovn-theme-emoji-btn");
        if (btn) updateEmoji(btn);
      });
    }
  }

  /* ----- Data Bus: Active Pulses (small cyan lights traveling down track with scroll) ----- */
  function initScrollProgress() {
    var container = document.getElementById("ovn-data-bus-pulses");
    var pulseCount = 4;
    if (!container) {
      container = document.createElement("div");
      container.id = "ovn-data-bus-pulses";
      container.setAttribute("aria-hidden", "true");
      document.body.appendChild(container);
      for (var i = 0; i < pulseCount; i++) {
        var pulse = document.createElement("div");
        pulse.className = "ovn-data-bus-pulse";
        pulse.style.left = (i % 2 === 0 ? "2px" : "6px");
        container.appendChild(pulse);
      }
    }
    var pulses = container.querySelectorAll(".ovn-data-bus-pulse");

    function update() {
      var h = document.documentElement.scrollHeight - window.innerHeight;
      if (h <= 0) {
        for (var j = 0; j < pulses.length; j++) {
          pulses[j].style.top = "50%";
        }
        document.documentElement.style.setProperty("--ovn-scroll-progress", "0");
        return;
      }
      var p = Math.min(1, Math.max(0, window.scrollY / h));
      var isWide = window.matchMedia("(min-width: 76.25em)").matches;
      if (isWide) {
        var minTop = 70;
        var maxTop = window.innerHeight - 24;
        var range = maxTop - minTop;
        for (var k = 0; k < pulses.length; k++) {
          var offset = (k / pulseCount) * 0.3;
          var pulseP = (p + offset) % 1.2;
          if (pulseP > 1) pulseP = 1;
          pulses[k].style.top = (minTop + pulseP * range) + "px";
          pulses[k].style.left = (k % 2 === 0 ? "2px" : "6px");
          pulses[k].style.width = "6px";
          pulses[k].style.height = "6px";
          pulses[k].style.borderRadius = "50%";
          pulses[k].style.bottom = "auto";
        }
        document.documentElement.style.setProperty("--ovn-scroll-progress", "");
        var mainDot = document.getElementById("ovn-wire-progress");
        if (mainDot) mainDot.style.display = "none";
        for (var n = 0; n < pulses.length; n++) {
          pulses[n].style.display = "block";
        }
      } else {
        document.documentElement.style.setProperty("--ovn-scroll-progress", String(p));
        for (var m = 0; m < pulses.length; m++) {
          pulses[m].style.display = "none";
        }
        var mainBar = document.getElementById("ovn-wire-progress");
        if (!mainBar) {
          mainBar = document.createElement("div");
          mainBar.id = "ovn-wire-progress";
          mainBar.className = "ovn-wire-progress";
          document.body.appendChild(mainBar);
        }
        mainBar.style.display = "block";
        mainBar.style.top = "auto";
        mainBar.style.bottom = "0";
        mainBar.style.left = "0";
        mainBar.style.width = "100%";
        mainBar.style.height = "3px";
        mainBar.style.borderRadius = "0";
      }
    }

    window.addEventListener("scroll", update, { passive: true });
    window.addEventListener("resize", update);
    update();
  }

  /* ----- Fetch text under current heading (h1, h2, or h3) ----- */
  function getTextUnderHeading(heading) {
    var text = "";
    var el = heading.nextElementSibling;
    while (el) {
      if (el.tagName === "H1" || el.tagName === "H2" || el.tagName === "H3") break;
      text += (el.textContent || "").trim() + "\n";
      el = el.nextElementSibling;
    }
    return text.trim();
  }

  /* Google Gemini Sparkle icon (simplified star/sparkle) */
  function geminiSparkleSvg() {
    return '<svg viewBox="0 0 24 24" fill="none" xmlns="http://www.w3.org/2000/svg" aria-hidden="true"><path d="M12 2L13.5 8.5L20 10L13.5 11.5L12 18L10.5 11.5L4 10L10.5 8.5L12 2Z" fill="currentColor"/><path d="M17 14L17.8 17L21 17.8L17.8 18.6L17 22L16.2 18.6L13 17.8L16.2 17L17 14Z" fill="currentColor" opacity="0.8"/></svg>';
  }

  /* ----- Gemini API: summarize with placeholder API_KEY ----- */
  function getGeminiApiKey() {
    if (typeof window !== "undefined" && window.OVN_GEMINI_API_KEY) return window.OVN_GEMINI_API_KEY;
    var script = document.querySelector('script[src*="theme.js"][data-gemini-api-key]');
    return script ? script.getAttribute("data-gemini-api-key") : "";
  }

  function summarizeWithGemini(sectionText, callback) {
    var apiKey = getGeminiApiKey();
    if (!apiKey) {
      callback("Gemini API key not set. Set window.OVN_GEMINI_API_KEY or data-gemini-api-key on the theme script.", null);
      return;
    }

    var prompt = "Summarize this technical OVN-Kubernetes documentation into 3 bullet points for a senior engineer.\n\n" + sectionText;
    var url = "https://generativelanguage.googleapis.com/v1beta/models/gemini-1.5-flash:generateContent?key=" + encodeURIComponent(apiKey);

    fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        contents: [{ parts: [{ text: prompt }] }],
        generationConfig: { maxOutputTokens: 256, temperature: 0.3 }
      })
    })
      .then(function (res) { return res.json(); })
      .then(function (data) {
        var text = null;
        if (data.candidates && data.candidates[0] && data.candidates[0].content && data.candidates[0].content.parts && data.candidates[0].content.parts[0]) {
          text = data.candidates[0].content.parts[0].text;
        }
        if (data.error) callback(data.error.message || "API error", null);
        else callback(null, text);
      })
      .catch(function (err) {
        callback(err.message || "Network error", null);
      });
  }

  /* ----- Gemini Summarizer: Sparkle icon, drawer with X + Copy ----- */
  function getOrCreateDrawer() {
    var drawer = document.getElementById("ovn-summary-drawer");
    if (drawer) return drawer;

    drawer = document.createElement("div");
    drawer.id = "ovn-summary-drawer";
    drawer.className = "ovn-summary-drawer";
    drawer.innerHTML =
      '<div class="ovn-summary-drawer__header">' +
        '<span class="ovn-summary-drawer__title">Gemini Summary</span>' +
        '<button type="button" class="ovn-summary-drawer__close" aria-label="Close">&times;</button>' +
      '</div>' +
      '<div class="ovn-summary-drawer__body"></div>' +
      '<div class="ovn-summary-drawer__actions">' +
        '<button type="button" class="ovn-summary-drawer__copy">Copy</button>' +
      '</div>';
    document.body.appendChild(drawer);

    function closeDrawer() {
      drawer.classList.remove("is-open");
      document.removeEventListener("keydown", drawer._onKey);
    }

    drawer.querySelector(".ovn-summary-drawer__close").addEventListener("click", closeDrawer);

    drawer._onKey = function (e) {
      if (e.key === "Escape") closeDrawer();
    };
    drawer._closeDrawer = closeDrawer;

    drawer.querySelector(".ovn-summary-drawer__copy").addEventListener("click", function () {
      var body = drawer.querySelector(".ovn-summary-drawer__body");
      if (body && body.textContent) {
        navigator.clipboard.writeText(body.textContent).then(function () {
          var copyBtn = drawer.querySelector(".ovn-summary-drawer__copy");
          copyBtn.textContent = "Copied!";
          setTimeout(function () { copyBtn.textContent = "Copy"; }, 1500);
        });
      }
    });

    return drawer;
  }

  function createGeminiButton(heading) {
    var btn = document.createElement("button");
    btn.type = "button";
    btn.className = "ovn-gemini-summarize-btn";
    btn.setAttribute("aria-label", "Summarize with Gemini");
    btn.setAttribute("title", "Summarize with Gemini");
    btn.innerHTML = '<span class="ovn-gemini-sparkle-wrap">' + geminiSparkleSvg() + "</span>";

    btn.addEventListener("click", function () {
      if (btn.hasAttribute("data-loading")) return;
      btn.setAttribute("data-loading", "true");

      var sectionText = getTextUnderHeading(heading);
      if (!sectionText) {
        sectionText = (heading.textContent || "").trim() + " (no body text)";
      }

      var drawer = getOrCreateDrawer();
      drawer.querySelector(".ovn-summary-drawer__body").textContent = "Loading…";
      drawer.classList.add("is-open");
      document.addEventListener("keydown", drawer._onKey);

      summarizeWithGemini(sectionText, function (err, result) {
        btn.removeAttribute("data-loading");
        var bodyEl = drawer.querySelector(".ovn-summary-drawer__body");
        if (err) {
          bodyEl.textContent = "Error: " + err + "\n\nSet window.OVN_GEMINI_API_KEY or data-gemini-api-key on the theme script to use Gemini.";
        } else {
          bodyEl.textContent = result || "No summary returned.";
        }
      });
    });

    return btn;
  }

  function initSummarizer() {
    var content = document.querySelector(".md-content__inner");
    if (!content) return;

    var headings = content.querySelectorAll("h1[id], h2[id], h3[id]");
    for (var i = 0; i < headings.length; i++) {
      var h = headings[i];
      if (h.querySelector(".ovn-gemini-summarize-btn")) continue;
      var btn = createGeminiButton(h);
      var link = h.querySelector(".headerlink");
      if (link) h.insertBefore(btn, link);
      else h.appendChild(btn);
    }

    var oldSummarize = content.querySelectorAll(".ovn-summarize-btn");
    for (var j = 0; j < oldSummarize.length; j++) oldSummarize[j].remove();
  }

  function run() {
    initFromStorage();
    document.addEventListener("DOMContentLoaded", function () {
      bindThemeToggle();
      setTimeout(createThemeEmojiBtn, 100);
      initScrollProgress();
      initSummarizer();

      var contentInner = document.querySelector(".md-content__inner");
      if (contentInner && contentInner.parentNode) {
        var obs = new MutationObserver(function (mutations) {
          for (var j = 0; j < mutations.length; j++) {
            if (mutations[j].type === "childList" && mutations[j].target === contentInner.parentNode) {
              setTimeout(initSummarizer, 50);
              break;
            }
          }
        });
        obs.observe(contentInner.parentNode, { childList: true });
      }
      document.addEventListener("navigation:loaded", function () {
        initScrollProgress();
        initSummarizer();
        var btn = document.getElementById("ovn-theme-emoji-btn");
        if (btn) updateEmoji(btn);
      });
    });
  }

  run();
})();
