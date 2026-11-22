export const htmlCases = [
  ['image attributes', '<img src="data:,review-image" alt="review-image" width="3">'],
  [
    'script attributes',
    '<script type="application/json" data-review="script">{"review":true}</script>',
  ],
  [
    'svg attributes',
    '<svg viewBox="0 0 1 1" aria-label="review-svg"><title>review-svg-title</title></svg>',
  ],
  ['iframe attributes', '<iframe src="about:blank" title="review-frame"></iframe>'],
] as const

export const deniedURLs = [
  ['lowercase script scheme', 'javascript:review_marker'],
  ['uppercase script scheme', 'JAVASCRIPT:review_marker'],
  ['mixed script scheme', 'JaVaScRiPt:review_marker'],
  ['tab in scheme', 'JaVa\tScRiPt:review_marker'],
  ['newline in scheme', 'JaVa\nScRiPt:review_marker'],
  ['space in scheme', 'JaVa ScRiPt:review_marker'],
  ['leading spaces', '  JaVaScRiPt:review_marker'],
  ['numeric entity in scheme', 'jav&#x61;script:review_marker'],
  ['decimal entity in scheme', 'jav&#97;script:review_marker'],
  ['named entity for colon', 'javascript&colon;review_marker'],
  ['entity tab in scheme', 'java&#9;script:review_marker'],
  ['data image', 'data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7'],
  ['mixed data scheme', 'DaTa:text/plain,review-marker'],
  ['uppercase file scheme', 'FILE:///review-marker'],
  ['mixed ftp scheme', 'FtP://example.invalid/review-marker'],
  ['tel scheme', 'TeL:000'],
  ['protocol entity', 'd&#97;ta:text/plain,review-marker'],
] as const

export const autoURLs = [
  'javascript:review_marker',
  'JaVaScRiPt:review_marker',
  'data:text/plain,review-marker',
  'ftp://example.invalid/review-marker',
  'file:///review-marker',
] as const
