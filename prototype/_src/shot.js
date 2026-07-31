const { chromium } = require('playwright');
const path = require('path');
const fs = require('fs');

const ROOT = path.resolve(__dirname, '..');
const OUT = path.join(ROOT, 'screenshots');
fs.mkdirSync(OUT, { recursive: true });

const PAGES = ['home', 'features', 'pricing', 'docs', 'contact'];

(async () => {
  const browser = await chromium.launch();
  const errors = [];

  for (const w of [1440, 390]) {
    const ctx = await browser.newContext({ viewport: { width: w, height: 900 }, deviceScaleFactor: 2 });
    for (const name of PAGES) {
      const page = await ctx.newPage();
      page.on('pageerror', e => errors.push(name + ' @' + w + ': ' + e.message));
      page.on('console', m => { if (m.type() === 'error') errors.push(name + ' console: ' + m.text()); });
      await page.goto('file://' + path.join(ROOT, name + '.html'));
      await page.waitForTimeout(700);
      // walk the page so every scroll-reveal block actually fires
      const h = await page.evaluate(() => document.body.scrollHeight);
      for (let y = 0; y < h; y += 500) {
        await page.evaluate(v => window.scrollTo(0, v), y);
        await page.waitForTimeout(90);
      }
      await page.waitForTimeout(700);
      await page.evaluate(() => window.scrollTo(0, 0));
      await page.waitForTimeout(500);
      const suffix = w === 1440 ? '' : '-mobile';
      await page.screenshot({ path: path.join(OUT, name + suffix + '.png'), fullPage: true });
      await page.close();
    }
    await ctx.close();
  }

  // interaction states on desktop
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 }, deviceScaleFactor: 2 });

  const p1 = await ctx.newPage();
  await p1.goto('file://' + path.join(ROOT, 'home.html'));
  await p1.waitForTimeout(800);
  await p1.keyboard.press('Meta+k');
  await p1.waitForTimeout(400);
  await p1.fill('#cmdk-input', 'roll');
  await p1.waitForTimeout(400);
  await p1.screenshot({ path: path.join(OUT, 'state-command-palette.png') });
  await p1.close();

  const p2 = await ctx.newPage();
  await p2.goto('file://' + path.join(ROOT, 'features.html'));
  await p2.waitForTimeout(800);
  await p2.click('[data-filter="edge"]');
  await p2.waitForTimeout(400);
  await p2.screenshot({ path: path.join(OUT, 'state-features-filtered.png'), fullPage: true });
  await p2.close();

  const p3 = await ctx.newPage();
  await p3.goto('file://' + path.join(ROOT, 'contact.html'));
  await p3.waitForTimeout(800);
  await p3.click('#ticket-form button[type="submit"]');
  await p3.waitForTimeout(600);
  await p3.screenshot({ path: path.join(OUT, 'state-form-validation.png') });
  await p3.close();

  const p4 = await ctx.newPage();
  await p4.goto('file://' + path.join(ROOT, 'pricing.html'));
  await p4.waitForTimeout(800);
  await p4.click('[data-value="annual"]');
  await p4.waitForTimeout(600);
  await p4.screenshot({ path: path.join(OUT, 'state-pricing-annual.png') });
  await p4.close();

  await ctx.close();
  await browser.close();

  if (errors.length) {
    console.log('ERRORS:');
    errors.forEach(e => console.log('  ' + e));
  } else {
    console.log('no page errors');
  }
  console.log(fs.readdirSync(OUT).sort().join('\n'));
})();
