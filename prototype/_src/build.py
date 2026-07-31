# -*- coding: utf-8 -*-
'''Compose the Relay Deploy prototype pages from the shared shell.'''
import pathlib
import sys

HERE = pathlib.Path(__file__).parent
ROOT = HERE.parent
LUCID = ROOT.parent / '.lucid' / 'design'
SLUG = 'product-requirements-document'
H = chr(35)

sys.path.insert(0, str(HERE))
import shell


def part(name):
    return (HERE / name).read_text().replace('_H_', H)


PAGES = [
    ('home', 'Home', 'Home / <b>Overview</b>', ['main_home_a.html', 'main_home_b.html'], ''),
    ('features', 'Features', 'Features / <b>Platform capabilities</b>', ['main_features_a.html', 'main_features_b.html', 'main_features_c.html'], ''),
    ('pricing', 'Pricing', 'Pricing / <b>Plan &amp; usage</b>', ['main_pricing_a.html', 'main_pricing_b.html'], 'pricing'),
    ('docs', 'Docs', 'Docs / Deployments / <b>Rollback</b>', ['main_docs.html'], 'docs'),
    ('contact', 'Contact', 'Contact / <b>Support</b>', ['main_contact_a.html', 'main_contact_b.html'], 'contact'),
]


def build():
    written = []
    for slug, title, crumb, parts, extra in PAGES:
        main = '\n'.join(part(p) for p in parts)
        body = ''
        if extra:
            body = '<script src="./assets/' + extra + '.js"></script>'
        html = shell.render(title, crumb, slug, main, '', body)
        out = ROOT / (slug + '.html')
        out.write_text(html)
        written.append((out, len(html)))

        lucid_name = SLUG + '.html' if slug == 'home' else SLUG + '--' + slug + '.html'
        target = LUCID / lucid_name
        if LUCID.exists():
            target.write_text(inline(html))
            written.append((target, target.stat().st_size))
    return written


def inline(html):
    '''Self-contained variant for the .lucid design session.'''
    assets = ROOT / 'assets'
    css = (assets / 'shell.css').read_text()
    html = html.replace('<link rel="stylesheet" href="./assets/shell.css">',
                        '<style>\n' + css + '\n</style>')
    for name in ('shell', 'pricing', 'docs', 'contact'):
        tag = '<script src="./assets/' + name + '.js"></script>'
        if tag in html:
            js = (assets / (name + '.js')).read_text()
            html = html.replace(tag, '<script>\n' + js + '\n</script>')
    return html


if __name__ == '__main__':
    for path, size in build():
        print(size, path)
