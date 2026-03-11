
## headful

| Operation                        |       Min |    Median |      Mean |       P95 |       Max |     N |
|----------------------------------|-----------|-----------|-----------|-----------|-----------|-------|
| **Navigation                    ** |           |           |           |           |           |       |
| Navigate[static]                 |    79.9ms |    79.9ms |    79.9ms |    79.9ms |    79.9ms |     1 |
| **Screenshot                    ** |           |           |           |           |           |       |
| Screenshot.JPEG.q80              |    62.1ms |   150.5ms |   161.4ms |   363.6ms |   363.6ms |     6 |
| Screenshot.PNG                   |    81.5ms |   165.7ms |   172.8ms |   308.5ms |   308.5ms |     6 |
| Screenshot.FullPage              |    97.1ms |   577.3ms |   412.4ms |   816.3ms |   816.3ms |     6 |
| Screenshot.ClipRegion            |    70.5ms |   106.1ms |   163.1ms |   497.8ms |   497.8ms |     6 |
| **JS Evaluation                 ** |           |           |           |           |           |       |
| Eval.Trivial                     |     1.8ms |     1.9ms |     2.7ms |     6.3ms |     6.3ms |     6 |
| Eval.QuerySelectorAll            |     1.4ms |     1.7ms |     2.2ms |     5.3ms |     5.3ms |     6 |
| Eval.InnerText                   |     1.6ms |     2.4ms |    10.1ms |    49.6ms |    49.6ms |     6 |
| Eval.GetComputedStyle            |     6.2ms |     6.9ms |     8.9ms |    14.6ms |    14.6ms |     6 |
| Eval.ScrollToBottom              |     1.5ms |     1.7ms |     2.3ms |     5.8ms |     5.8ms |     6 |
| Eval.DOMManipulation             |     1.8ms |     1.9ms |     2.0ms |     2.3ms |     2.3ms |     6 |
| Eval.BoundingRects               |     1.7ms |     2.0ms |     2.5ms |     5.2ms |     5.2ms |     6 |
| **DOM                           ** |           |           |           |           |           |       |
| DOM.GetDocument.Shallow          |     1.5ms |     1.7ms |     3.0ms |    10.0ms |    10.0ms |     6 |
| DOM.GetDocument.Deep             |     1.6ms |     5.1ms |     6.9ms |    25.7ms |    25.7ms |     6 |
| DOM.GetDocument.Full             |     1.6ms |    47.3ms |    46.6ms |   124.4ms |   124.4ms |     6 |
| DOM.QuerySelector                |     1.5ms |     1.8ms |     3.1ms |    10.2ms |    10.2ms |     6 |
| DOM.GetOuterHTML                 |     1.5ms |    31.4ms |    19.8ms |    41.3ms |    41.3ms |     6 |
| **Input                         ** |           |           |           |           |           |       |
| Input.MouseMove                  |     3.2ms |    29.4ms |    20.2ms |    43.3ms |    43.3ms |     6 |
| Input.Click                      |     4.2ms |     5.3ms |     8.1ms |    22.7ms |    22.7ms |     6 |
| Input.TypeText                   |    56.6ms |    65.3ms |   104.4ms |   205.0ms |   205.0ms |     6 |
| Input.Scroll                     |   171.2ms |   179.0ms |   299.0ms |   770.3ms |   770.3ms |     6 |
| **Network                       ** |           |           |           |           |           |       |
| Network.GetCookies               |     1.4ms |     2.0ms |     2.2ms |     4.6ms |     4.6ms |     6 |
| Network.GetResponseBody          |     6.0ms |    68.8ms |    68.5ms |   218.6ms |   218.6ms |     6 |
| **Page                          ** |           |           |           |           |           |       |
| Page.Reload                      |    60.8ms |   104.8ms |   244.0ms |   655.2ms |   655.2ms |     6 |
| Page.GetNavigationHistory        |     1.5ms |     1.7ms |     2.3ms |     5.2ms |     5.2ms |     6 |
| Page.GetLayoutMetrics            |     1.8ms |     8.7ms |    16.1ms |    54.5ms |    54.5ms |     6 |
| Page.PrintToPDF                  |    73.1ms |   287.0ms |   442.5ms |     1.26s |     1.26s |     6 |
| **Emulation                     ** |           |           |           |           |           |       |
| Emulation.SetViewport            |     1.7ms |     1.9ms |     2.1ms |     3.1ms |     3.1ms |     6 |
| Emulation.SetMobile              |    16.4ms |    57.2ms |   102.4ms |   413.9ms |   413.9ms |     6 |
| Emulation.SetGeolocation         |     1.4ms |     1.4ms |     1.6ms |     2.3ms |     2.3ms |     6 |
| **Target                        ** |           |           |           |           |           |       |
| Target.GetTargets                |     1.3ms |     1.5ms |     1.6ms |     2.2ms |     2.2ms |     6 |
| Target.CreateAndClose            |    20.5ms |    35.3ms |    31.6ms |    40.3ms |    40.3ms |     6 |
| **Composite                     ** |           |           |           |           |           |       |
| Composite.NavAndScreenshot       |   222.0ms |   277.6ms |   304.2ms |   535.2ms |   535.2ms |     6 |
| Composite.ScrapeLinks            |     2.3ms |     3.1ms |     5.2ms |    11.1ms |    11.1ms |     6 |
| Composite.FillForm               |    11.6ms |    12.7ms |    13.2ms |    15.8ms |    15.8ms |     6 |
| Composite.ClickAndWait           |    26.9ms |    43.8ms |    47.5ms |    79.9ms |    79.9ms |     6 |
| Composite.RapidScreenshots       |   971.0ms |     1.14s |     1.10s |     1.24s |     1.24s |     6 |
| Composite.ScrollAndCapture       |   599.8ms |   606.5ms |   617.7ms |   655.0ms |   655.0ms |     6 |
| **Navigation                    ** |           |           |           |           |           |       |
| Navigate[content]                |   156.9ms |   225.6ms |   206.8ms |   238.0ms |   238.0ms |     3 |
| Navigate[spa]                    |     1.12s |     1.12s |     1.12s |     1.12s |     1.12s |     1 |
| Navigate[media]                  |     1.08s |     1.08s |     1.08s |     1.08s |     1.08s |     1 |
| **Concurrent                    ** |           |           |           |           |           |       |
| Concurrent.DOM                   |     1.3ms |     1.9ms |     3.9ms |     7.2ms |    52.5ms |   337 |
| Concurrent.Evaluate              |     1.3ms |    10.6ms |    15.5ms |    33.1ms |   133.3ms |   337 |
| Concurrent.Screenshot            |    81.3ms |   153.6ms |   158.1ms |   223.5ms |   266.3ms |   337 |
