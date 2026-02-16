/**
 * OVN-Kubernetes docs — Additional UX (Step 2).
 * Breadcrumb, reading progress, related pages, scroll highlight, fade-in, glossary.
 * Extends MkDocs Material; no SPA, minimal JS only.
 */
(function () {
  "use strict";

  /* ----- Reading progress indicator (top bar) ----- */
  function initReadingProgress() {
    var bar = document.getElementById("ovn-reading-progress-bar");
    if (!bar) {
      var wrap = document.createElement("div");
      wrap.className = "ovn-reading-progress";
      wrap.setAttribute("aria-hidden", "true");
      wrap.innerHTML = '<div class="ovn-reading-progress__bar" id="ovn-reading-progress-bar"></div>';
      var header = document.querySelector(".md-header");
      if (header && header.nextSibling) {
        header.parentNode.insertBefore(wrap, header.nextSibling);
      } else {
        document.body.insertBefore(wrap, document.body.firstChild);
      }
      bar = document.getElementById("ovn-reading-progress-bar");
    }
    function update() {
      var h = document.documentElement.scrollHeight - window.innerHeight;
      if (h <= 0) {
        bar.style.width = "0%";
        return;
      }
      var p = (window.scrollY / h) * 100;
      bar.style.width = p + "%";
    }
    window.addEventListener("scroll", update, { passive: true });
    window.addEventListener("resize", update);
    update();
  }

  /* ----- Breadcrumb below header ----- */
  function initBreadcrumb() {
    var container = document.getElementById("ovn-breadcrumb");
    if (container) return;
    var tabs = document.querySelector(".md-tabs");
    if (!tabs) return;
    container = document.createElement("nav");
    container.id = "ovn-breadcrumb";
    container.className = "ovn-breadcrumb";
    container.setAttribute("aria-label", "Breadcrumb");
    var title = document.querySelector(".md-content__inner h1");
    var path = window.location.pathname.replace(/\/$/, "").split("/").filter(Boolean);
    var base = "";
    var parts = [];
    path.forEach(function (seg, i) {
      base += "/" + seg;
      var label = seg.replace(/-/g, " ").replace(/\b\w/g, function (c) { return c.toUpperCase(); });
      if (i === path.length - 1) {
        parts.push('<span class="ovn-breadcrumb__current">' + (title ? title.textContent.trim().replace(/</g, "&lt;") : label) + "</span>");
      } else {
        parts.push('<a href="' + base + '/">' + label + "</a>");
      }
    });
    if (parts.length === 0) {
      parts.push('<span class="ovn-breadcrumb__current">' + (document.title || "Home") + "</span>");
    }
    container.innerHTML = parts.join('<span class="ovn-breadcrumb__sep">/</span>');
    tabs.parentNode.insertBefore(container, tabs.nextSibling);
  }

  /* ----- Related Pages at bottom ----- */
  function initRelatedPages() {
    var content = document.querySelector(".md-content__inner");
    if (!content || document.getElementById("ovn-related-pages")) return;
    var nav = document.querySelector(".md-nav--primary .md-nav__list");
    if (!nav) return;
    var current = document.querySelector(".md-nav__link--active");
    if (!current) return;
    var parent = current.closest(".md-nav__item--nested");
    var links = [];
    if (parent) {
      var items = parent.querySelectorAll(".md-nav__link");
      items.forEach(function (a) {
        if (a.href && a !== current && a.textContent.trim()) {
          links.push({ href: a.href, text: a.textContent.trim() });
        }
      });
    }
    if (links.length === 0) {
      var all = nav.querySelectorAll(".md-nav__link[href]");
      for (var i = 0; i < Math.min(5, all.length); i++) {
        var a = all[i];
        if (a.href !== window.location.href && a.textContent.trim()) {
          links.push({ href: a.href, text: a.textContent.trim() });
        }
      }
    }
    if (links.length === 0) return;
    var block = document.createElement("div");
    block.id = "ovn-related-pages";
    block.className = "ovn-related-pages";
    block.innerHTML = '<div class="ovn-related-pages__title">Related Pages</div><ul class="ovn-related-pages__list"></ul>';
    var list = block.querySelector("ul");
    links.slice(0, 6).forEach(function (item) {
      var li = document.createElement("li");
      li.innerHTML = '<a href="' + item.href + '">' + item.text + "</a>";
      list.appendChild(li);
    });
    content.appendChild(block);
  }

  /* ----- Scroll-based section highlighting + fade-in ----- */
  function initScrollHighlight() {
    var content = document.querySelector(".md-content__inner");
    if (!content) return;
    var headings = content.querySelectorAll("h2[id], h3[id]");
    if (headings.length === 0) return;
    var observer = new IntersectionObserver(
      function (entries) {
        entries.forEach(function (entry) {
          if (entry.isIntersecting) {
            entry.target.classList.add("ovn-section-visible");
            entry.target.classList.add("ovn-fade-in", "ovn-in-view");
          }
        });
      },
      { rootMargin: "-10% 0px -60% 0px", threshold: 0 }
    );
    headings.forEach(function (h) {
      h.classList.add("ovn-fade-in");
      observer.observe(h);
    });
  }

  /* ----- Homepage hero + feature cards (always on intro; re-inject on back) ----- */
  function isIntroPage() {
    var path = (window.location.pathname || "/").replace(/\/$/, "") || "";
    return path === "" || path === "ovn-kubernetes";
  }

  function initHomepageHero() {
    var main = document.querySelector(".md-content__inner");
    if (!main || !main.querySelector("h1")) return;
    if (!isIntroPage()) return;
    var hero = document.getElementById("ovn-hero");
    if (hero) return;
    hero = document.createElement("div");
    hero.id = "ovn-hero";
    hero.className = "ovn-hero";
    var cards = document.createElement("div");
    cards.className = "ovn-feature-cards";
    cards.innerHTML =
      '<a href="design/architecture/" class="ovn-feature-card"><span class="ovn-feature-card__title">Architecture</span><span class="ovn-feature-card__desc">Control plane and data plane</span></a>' +
      '<a href="installation/launching-ovn-kubernetes-on-kind/" class="ovn-feature-card"><span class="ovn-feature-card__title">Installation</span><span class="ovn-feature-card__desc">KIND and Helm</span></a>' +
      '<a href="getting-started/configuration/" class="ovn-feature-card"><span class="ovn-feature-card__title">Configuration</span><span class="ovn-feature-card__desc">Setup and CLI</span></a>' +
      '<a href="features/user-defined-networks/user-defined-networks/" class="ovn-feature-card"><span class="ovn-feature-card__title">Features</span><span class="ovn-feature-card__desc">UDN, Egress, BGP</span></a>';
    hero.appendChild(cards);
    var h1 = main.querySelector("h1");
    if (h1 && h1.parentNode) {
      h1.parentNode.insertBefore(hero, h1.nextSibling);
    } else {
      main.insertBefore(hero, main.firstChild);
    }
  }

  function ensureIntroBoxes() {
    if (!isIntroPage()) return;
    setTimeout(initHomepageHero, 0);
    setTimeout(initHomepageHero, 200);
    setTimeout(initHomepageHero, 500);
  }

  /* ----- Glossary tooltips (data-glossary="definition") ----- */
  function initGlossary() {
    document.querySelectorAll("[data-glossary]").forEach(function (el) {
      if (el.querySelector(".ovn-glossary-tooltip")) return;
      var def = el.getAttribute("data-glossary");
      if (!def) return;
      var tip = document.createElement("span");
      tip.className = "ovn-glossary-tooltip";
      tip.textContent = def;
      el.style.position = "relative";
      el.appendChild(tip);
    });
  }

  function run() {
    if (document.readyState === "loading") {
      document.addEventListener("DOMContentLoaded", run);
      return;
    }
    initReadingProgress();
    initBreadcrumb();
    initRelatedPages();
    initScrollHighlight();
    initHomepageHero();
    initGlossary();
    document.addEventListener("navigation:loaded", function () {
      initBreadcrumb();
      initRelatedPages();
      initScrollHighlight();
      initHomepageHero();
      initGlossary();
      if (isIntroPage()) {
        setTimeout(initHomepageHero, 100);
        setTimeout(initHomepageHero, 400);
      }
    });
    window.addEventListener("popstate", ensureIntroBoxes);
  }

  run();
})();
