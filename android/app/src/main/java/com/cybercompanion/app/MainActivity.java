package com.cybercompanion.app;

import android.Manifest;
import android.content.Intent;
import android.net.Uri;
import android.content.pm.PackageManager;
import android.graphics.Bitmap;
import android.os.Build;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.util.Log;
import android.view.View;
import android.webkit.WebResourceError;
import android.webkit.WebResourceRequest;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.Button;
import android.widget.ProgressBar;
import android.widget.TextView;

import androidx.appcompat.app.AppCompatActivity;

import java.net.HttpURLConnection;
import java.net.URL;

public class MainActivity extends AppCompatActivity {
    private static final String TAG = "MainActivity";
    private static final String DASHBOARD_URL = "http://127.0.0.1:8088";
    private WebView webView;
    private ProgressBar progressBar;
    private TextView loadingText;
    private Button retryButton;
    private Handler handler = new Handler(Looper.getMainLooper());
    private boolean isLoaded = false;

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        setContentView(R.layout.activity_main);

        webView = findViewById(R.id.webview);
        progressBar = findViewById(R.id.progressBar);
        loadingText = findViewById(R.id.loadingText);
        retryButton = findViewById(R.id.retryButton);

        if (retryButton != null) {
            retryButton.setOnClickListener(v -> {
                retryButton.setVisibility(View.GONE);
                progressBar.setVisibility(View.VISIBLE);
                loadingText.setText("正在重试连接...");
                startCoreService();
                checkAndLoad();
            });
        }

        // Request notification permission on Android 13+
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            if (checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) {
                requestPermissions(new String[]{Manifest.permission.POST_NOTIFICATIONS}, 101);
            }
        }

        // 1. Start Background Core Service
        startCoreService();

        // 2. Setup WebView
        setupWebView();

        // 3. Poll and Load
        checkAndLoad();
    }

    private void startCoreService() {
        try {
            Intent serviceIntent = new Intent(this, BotService.class);
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
                startForegroundService(serviceIntent);
            } else {
                startService(serviceIntent);
            }
        } catch (Throwable e) {
            Log.e(TAG, "Failed to start BotService", e);
        }
    }

    private void setupWebView() {
        WebSettings settings = webView.getSettings();
        settings.setJavaScriptEnabled(true);
        settings.setDomStorageEnabled(true);
        settings.setDatabaseEnabled(true);
        settings.setUseWideViewPort(true);
        settings.setLoadWithOverviewMode(true);
        settings.setCacheMode(WebSettings.LOAD_DEFAULT);

        // ── WebView 安全加固 ──────────────────────────────────────────────
        // 面板是本地回环页面，不需要访问文件系统或跨源资源。
        // 关闭这些开关可避免恶意页面通过 file:// 读取应用私有数据。
        settings.setAllowFileAccess(false);
        settings.setAllowContentAccess(false);
        settings.setAllowFileAccessFromFileURLs(false);
        settings.setAllowUniversalAccessFromFileURLs(false);
        // 本地页面为 http://127.0.0.1，禁止其加载 https 之外的混合内容
        settings.setMixedContentMode(WebSettings.MIXED_CONTENT_NEVER_ALLOW);
        settings.setGeolocationEnabled(false);
        settings.setSaveFormData(false);
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            settings.setSafeBrowsingEnabled(true);
        }

        webView.setWebViewClient(new WebViewClient() {
            @Override
            public boolean shouldOverrideUrlLoading(WebView view, String url) {
                // 只允许停留在本地回环面板；任何外部跳转一律交给系统浏览器，
                // 避免面板页面被导航到恶意站点后继承 WebView 的 JS 权限。
                if (isLocalPanelUrl(url)) {
                    return false;
                }
                try {
                    startActivity(new Intent(Intent.ACTION_VIEW, Uri.parse(url)));
                } catch (Exception e) {
                    Log.w(TAG, "无法打开外部链接: " + url, e);
                }
                return true;
            }

            @Override
            public void onPageStarted(WebView view, String url, Bitmap favicon) {
                super.onPageStarted(view, url, favicon);
            }

            @Override
            public void onPageFinished(WebView view, String url) {
                super.onPageFinished(view, url);
                if (url.contains(":8088")) {
                    isLoaded = true;
                    progressBar.setVisibility(View.GONE);
                    loadingText.setVisibility(View.GONE);
                    if (retryButton != null) retryButton.setVisibility(View.GONE);
                    webView.setVisibility(View.VISIBLE);
                }
            }

            @Override
            public void onReceivedError(WebView view, WebResourceRequest request, WebResourceError error) {
                if (request.isForMainFrame()) {
                    Log.w(TAG, "WebView load error: " + error);
                }
            }
        });
    }

    private void checkAndLoad() {
        new Thread(() -> {
            boolean ready = false;
            for (int i = 0; i < 30; i++) {
                try {
                    URL u = new URL(DASHBOARD_URL);
                    HttpURLConnection conn = (HttpURLConnection) u.openConnection();
                    conn.setConnectTimeout(600);
                    conn.setReadTimeout(600);
                    conn.setRequestMethod("GET");
                    int code = conn.getResponseCode();
                    conn.disconnect();
                    if (code == 200) {
                        ready = true;
                        break;
                    }
                } catch (Exception ignored) {
                }
                try {
                    Thread.sleep(800);
                } catch (InterruptedException e) {
                    break;
                }
            }

            boolean finalReady = ready;
            handler.post(() -> {
                if (finalReady) {
                    webView.loadUrl(DASHBOARD_URL);
                } else {
                    if (!isLoaded) {
                        progressBar.setVisibility(View.GONE);
                        loadingText.setText("服务状态：" + BotService.getStatusMessage());
                        if (retryButton != null) {
                            retryButton.setVisibility(View.VISIBLE);
                        }
                    }
                }
            });
        }).start();
    }

    @Override
    public void onBackPressed() {
        if (webView.canGoBack()) {
            webView.goBack();
        } else {
            super.onBackPressed();
        }
    }

    /** 判断 URL 是否指向本机面板（仅 127.0.0.1 / localhost 的 8088 端口）。 */
    private boolean isLocalPanelUrl(String url) {
        if (url == null) return false;
        return url.startsWith("http://127.0.0.1:8088")
                || url.startsWith("http://localhost:8088")
                || url.equals("about:blank");
    }

}
