
## baseline

| Operation                        |       Min |    Median |      Mean |       P95 |       Max |     N |
|----------------------------------|-----------|-----------|-----------|-----------|-----------|-------|
| **Navigation                    ** |           |           |           |           |           |       |
| Navigate[static]                 |    22.7ms |    22.7ms |    22.7ms |    22.7ms |    22.7ms |     1 |
| **Screenshot                    ** |           |           |           |           |           |       |
| Screenshot.JPEG.q80              |    48.6ms |    65.4ms |   100.9ms |   305.8ms |   305.8ms |     6 |
| Screenshot.PNG                   |    62.5ms |   112.2ms |   120.3ms |   215.2ms |   215.2ms |     6 |
| Screenshot.FullPage              |    67.1ms |   462.7ms |   375.6ms |   841.0ms |   841.0ms |     6 |
| Screenshot.ClipRegion            |    24.5ms |    36.3ms |    82.6ms |   229.0ms |   229.0ms |     6 |
| **JS Evaluation                 ** |           |           |           |           |           |       |
| Eval.Trivial                     |     511µs |     570µs |    12.8ms |    70.4ms |    70.4ms |     6 |
| Eval.QuerySelectorAll            |     311µs |     401µs |    12.1ms |    70.7ms |    70.7ms |     6 |
| Eval.InnerText                   |     482µs |     742µs |     1.3ms |     3.8ms |     3.8ms |     6 |
| Eval.GetComputedStyle            |     3.9ms |     4.5ms |     5.2ms |     9.8ms |     9.8ms |     6 |
| Eval.ScrollToBottom              |     284µs |     371µs |     426µs |     774µs |     774µs |     6 |
| Eval.DOMManipulation             |     497µs |     629µs |     660µs |     1.0ms |     1.0ms |     6 |
| Eval.BoundingRects               |     428µs |     829µs |     840µs |     1.5ms |     1.5ms |     6 |
| **DOM                           ** |           |           |           |           |           |       |
| DOM.GetDocument.Shallow          |     228µs |     295µs |     752µs |     3.1ms |     3.1ms |     6 |
| DOM.GetDocument.Deep             |     272µs |     3.0ms |     2.4ms |     5.6ms |     5.6ms |     6 |
| DOM.GetDocument.Full             |     231µs |    28.6ms |    22.5ms |    58.1ms |    58.1ms |     6 |
| DOM.QuerySelector                |     207µs |     291µs |     1.7ms |     9.1ms |     9.1ms |     6 |
| DOM.GetOuterHTML                 |     171µs |    20.8ms |    12.3ms |    27.1ms |    27.1ms |     6 |
| **Input                         ** |           |           |           |           |           |       |
| Input.MouseMove                  |     1.3ms |     6.9ms |     7.1ms |    12.6ms |    12.6ms |     6 |
| Input.Click                      |     948µs |     3.1ms |     6.9ms |    28.5ms |    28.5ms |     6 |
| Input.TypeText                   |     7.4ms |     9.8ms |    70.3ms |   201.0ms |   201.0ms |     6 |
| Input.Scroll                     |    72.7ms |    81.6ms |    95.0ms |   156.0ms |   156.0ms |     6 |
| **Network                       ** |           |           |           |           |           |       |
| Network.GetCookies               |     400µs |     635µs |     1.1ms |     3.4ms |     3.4ms |     6 |
| Network.GetResponseBody          |     4.6ms |    66.1ms |   145.9ms |   667.6ms |   667.6ms |     6 |
| **Page                          ** |           |           |           |           |           |       |
| Page.Reload                      |    27.7ms |   158.3ms |   364.0ms |     1.27s |     1.27s |     6 |
| Page.GetNavigationHistory        |     1.0ms |     1.2ms |     1.5ms |     3.5ms |     3.5ms |     6 |
| Page.GetLayoutMetrics            |     390µs |     4.2ms |     9.0ms |    34.9ms |    34.9ms |     6 |
| Page.PrintToPDF                  |    30.0ms |   237.8ms |   373.9ms |     1.56s |     1.56s |     6 |
| **Emulation                     ** |           |           |           |           |           |       |
| Emulation.SetViewport            |     268µs |     469µs |     417µs |     548µs |     548µs |     6 |
| Emulation.SetMobile              |     1.3ms |   114.4ms |   126.1ms |   457.8ms |   457.8ms |     6 |
| Emulation.SetGeolocation         |      95µs |     173µs |     163µs |     221µs |     221µs |     6 |
| **Target                        ** |           |           |           |           |           |       |
| Target.GetTargets                |     115µs |     143µs |     137µs |     150µs |     150µs |     6 |
| Target.CreateAndClose            |    10.7ms |    14.7ms |    14.9ms |    20.2ms |    20.2ms |     6 |
| **Composite                     ** |           |           |           |           |           |       |
| Composite.NavAndScreenshot       |    98.7ms |   135.3ms |   158.4ms |   282.8ms |   282.8ms |     6 |
| Composite.ScrapeLinks            |     3.5ms |     4.0ms |     3.9ms |     4.5ms |     4.5ms |     6 |
| Composite.FillForm               |     5.4ms |     7.9ms |     7.3ms |     8.8ms |     8.8ms |     6 |
| Composite.ClickAndWait           |    22.5ms |    26.3ms |    25.2ms |    27.4ms |    27.4ms |     6 |
| Composite.RapidScreenshots       |   594.7ms |   647.1ms |   643.8ms |   682.5ms |   682.5ms |     6 |
| Composite.ScrollAndCapture       |   499.3ms |   502.9ms |   505.6ms |   523.3ms |   523.3ms |     6 |
| **Navigation                    ** |           |           |           |           |           |       |
| Navigate[content]                |   106.7ms |   111.8ms |   166.3ms |   280.3ms |   280.3ms |     3 |
| Navigate[spa]                    |     1.44s |     1.44s |     1.44s |     1.44s |     1.44s |     1 |
| Navigate[media]                  |     1.52s |     1.52s |     1.52s |     1.52s |     1.52s |     1 |
| **Concurrent                    ** |           |           |           |           |           |       |
| Concurrent.DOM                   |     200µs |     365µs |     383µs |     599µs |     927µs |   471 |
| Concurrent.Evaluate              |     192µs |    21.2ms |    20.7ms |    42.1ms |    53.6ms |   471 |
| Concurrent.Screenshot            |    56.9ms |   107.0ms |   106.8ms |   134.1ms |   148.9ms |   471 |
