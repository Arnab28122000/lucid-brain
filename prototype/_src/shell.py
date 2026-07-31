import pathlib
# -*- coding: utf-8 -*-
'''Shared app-shell template for the Relay Deploy prototype.'''

H = chr(35)

NAV = [
    ('home', 'Home', './home.html', '',
     '<path d="M3 10.5 12 4l9 6.5V20a1 1 0 0 1-1 1h-5v-6H9v6H4a1 1 0 0 1-1-1z"/>'),
    ('features', 'Features', './features.html', '14',
     '<path d="M12 3l9 5-9 5-9-5z"/><path d="M3 12l9 5 9-5"/><path d="M3 16l9 5 9-5"/>'),
    ('pricing', 'Pricing', './pricing.html', '',
     '<path d="M20.6 13.4 12 22l-9-9V4h9z"/><circle cx="7.5" cy="7.5" r="1.4"/>'),
    ('docs', 'Docs', './docs.html', '',
     '<path d="M4 4h9a3 3 0 0 1 3 3v13a2 2 0 0 0-2-2H4z"/><path d="M20 4h-1a3 3 0 0 0-3 3v13a2 2 0 0 1 2-2h2z"/>'),
    ('contact', 'Contact', './contact.html', '3',
     '<rect x="3" y="5" width="18" height="14" rx="2"/><path d="m3.5 7 8.5 6 8.5-6"/>'),
]


def nav(active):
    out = []
    for slug, label, href, tail, icon in NAV:
        cls = ' class="active"' if slug == active else ''
        cur = ' aria-current="page"' if slug == active else ''
        tl = '<span class="tail">' + tail + '</span>' if tail else ''
        out.append(
            '        <a href="' + href + '"' + cls + cur + '>\n'
            '          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" '
            'stroke="currentColor" stroke-width="1.6" aria-hidden="true">' + icon + '</svg>\n'
            '          ' + label + tl + '\n'
            '        </a>'
        )
    return '\n'.join(out)


PAGE = (pathlib.Path(__file__).parent / 'page_tpl.html').read_text()


def render(title, crumb, active, main, headextra='', bodyextra=''):
    return (PAGE
            .replace('__TITLE__', title)
            .replace('__CRUMB__', crumb)
            .replace('__NAV__', nav(active))
            .replace('__MAIN__', main)
            .replace('__HEADEXTRA__', headextra)
            .replace('__BODYEXTRA__', bodyextra))
