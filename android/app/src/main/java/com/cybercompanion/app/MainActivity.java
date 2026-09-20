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
import android.webkit.CookieManager;
import android.webkit.WebResourceError;
import android.webkit.WebResourceRequest;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.Button;
import android.widget.ProgressBar;
import android.widget.TextView;

import androidx.appcompat.app.AppCompatActivity;

import org.json.JSONObject;

import java.io.ByteArrayOutputStream;
import java.io.File;
import java.io.FileInputStream;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;

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

        // 首次启动一次性申请所需运行时权限。用户拒绝不影响主流程，
        // 只是硬件面板上会少一到两项数据。
        requestRuntimePermissions();

        // 申请电池优化白名单：国产 ROM 会在息屏数分钟后强杀后台服务，
        // 不在白名单里时"24 小时在线"无法实现（优化建议书 7.1 ①）。
        requestIgnoreBatteryOptimization();

        // 1. Start Background Core Service
        startCoreService();

        // 2. Setup WebView
        setupWebView();

        // 3. Poll and Load
        checkAndLoad();
    }

    /**
     * 申请运行时权限。
     *
     * <ul>
     *   <li>POST_NOTIFICATIONS（Android 13+）：前台服务常驻通知，没它服务会被系统降级；</li>
     *   <li>READ_PHONE_STATE：蜂窝制式（5G NR / 4G LTE）与信号强度 dBm。
     *       安卓把这些数据列为受保护字段，没有该权限时只能显示"已连网"。</li>
     * </ul>
     */
    private void requestRuntimePermissions() {
        List<String> wanted = new ArrayList<>();
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU
                && checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS)
                != PackageManager.PERMISSION_GRANTED) {
            wanted.add(Manifest.permission.POST_NOTIFICATIONS);
        }
        if (checkSelfPermission(Manifest.permission.READ_PHONE_STATE)
                != PackageManager.PERMISSION_GRANTED) {
            wanted.add(Manifest.permission.READ_PHONE_STATE);
        }
        if (wanted.isEmpty()) {
            return;
        }
        try {
            requestPermissions(wanted.toArray(new String[0]), 101);
        } catch (Throwable e) {
            // 无电话模块的设备上申请该权限可能直接抛异常，忽略即可
            Log.w(TAG, "申请运行时权限失败: " + e.getMessage());
        }
    }

    /**
     * 引导用户把本应用加入电池优化白名单。
     *
     * 这一步是"24 小时在线"能否成立的关键：未加入白名单时，
     * 小米 / 华为 / OPPO 等 ROM 会在息屏后几分钟内回收前台服务，
     * 而 JobScheduler 最快也要 15 分钟才拉一次，中间的空窗用户能直接感知到。
     */
    private void requestIgnoreBatteryOptimization() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.M) {
            return;
        }
        try {
            android.os.PowerManager pm = (android.os.PowerManager) getSystemService(POWER_SERVICE);
            if (pm == null || pm.isIgnoringBatteryOptimizations(getPackageName())) {
                return;
            }
            Intent intent = new Intent(android.provider.Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS);
            intent.setData(Uri.parse("package:" + getPackageName()));
            startActivity(intent);
        } catch (Throwable e) {
            // 部分 ROM 不支持该设置页，忽略即可（用户仍可在系统设置中手动加白名单）
            Log.w(TAG, "无法跳转电池优化设置页: " + e.getMessage());
        }
    }

    private void startCoreService() {
        // 注册 JobScheduler 兜底拉活：前台服务在国产 ROM 上仍可能被清理，
        // 由系统统一调度的 Job 是官方认可的恢复入口。
        KeepAliveJobService.schedule(this);
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
            // 本机自动登录：Go 核心与本 App 同设备同私有目录，属于同一信任域，
            // 因此可以直接用配置文件里已保存的密码换取会话，免去每次打开都输密码。
            // 密码尚未创建（首次运行）时返回 null，面板会显示创建密码引导。
            String password = finalReady ? readPanelPassword() : null;
            String cookieHeader = (finalReady && password != null) ? loginAndGetCookie(password) : null;

            handler.post(() -> {
                if (finalReady) {
                    if (cookieHeader != null) {
                        injectSessionCookies(cookieHeader);
                    }
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

    /**
     * 读取本机配置中的面板密码。
     *
     * 返回 null 表示「尚未创建密码」或读取失败 —— 这两种情况都交给面板处理：
     * 前者显示创建密码引导，后者显示普通登录框。
     */
    private String readPanelPassword() {
        FileInputStream in = null;
        try {
            File cfg = new File(getFilesDir(), "config.json");
            if (!cfg.exists()) {
                return null;
            }
            in = new FileInputStream(cfg);
            ByteArrayOutputStream out = new ByteArrayOutputStream();
            byte[] buf = new byte[8192];
            int n;
            while ((n = in.read(buf)) > 0) {
                out.write(buf, 0, n);
            }
            JSONObject obj = new JSONObject(new String(out.toByteArray(), StandardCharsets.UTF_8));
            String pwd = obj.optString("web_password", "");
            return pwd.isEmpty() ? null : pwd;
        } catch (Throwable e) {
            Log.w(TAG, "读取面板密码失败，将显示登录页: " + e.getMessage());
            return null;
        } finally {
            if (in != null) {
                try {
                    in.close();
                } catch (Throwable ignored) {
                }
            }
        }
    }

    /** 用面板密码换取会话，返回原始 Set-Cookie 值（分号分隔）；失败返回 null。 */
    private String loginAndGetCookie(String password) {
        try {
            HttpURLConnection conn = (HttpURLConnection) new URL(DASHBOARD_URL + "/api/login").openConnection();
            conn.setConnectTimeout(3000);
            conn.setReadTimeout(3000);
            conn.setRequestMethod("POST");
            conn.setDoOutput(true);
            conn.setRequestProperty("Content-Type", "application/json");
            // 显式带同源 Origin，满足服务端的跨站请求校验
            conn.setRequestProperty("Origin", DASHBOARD_URL);
            conn.setInstanceFollowRedirects(false);

            String body = "{\"password\":\"" + jsonEscape(password) + "\"}";
            OutputStream os = conn.getOutputStream();
            os.write(body.getBytes(StandardCharsets.UTF_8));
            os.close();

            int code = conn.getResponseCode();
            // 响应头字段名大小写不敏感，这里显式遍历以免依赖具体实现
            List<String> cookies = null;
            Map<String, List<String>> headers = conn.getHeaderFields();
            if (headers != null) {
                for (Map.Entry<String, List<String>> e : headers.entrySet()) {
                    if (e.getKey() != null && "Set-Cookie".equalsIgnoreCase(e.getKey())) {
                        cookies = e.getValue();
                        break;
                    }
                }
            }
            conn.disconnect();

            if (code != HttpURLConnection.HTTP_OK || cookies == null || cookies.isEmpty()) {
                Log.w(TAG, "自动登录未成功，将显示登录页 (HTTP " + code + ")");
                return null;
            }
            StringBuilder sb = new StringBuilder();
            for (String c : cookies) {
                int idx = c.indexOf(';');
                String pair = idx > 0 ? c.substring(0, idx) : c;
                if (sb.length() > 0) sb.append("; ");
                sb.append(pair.trim());
            }
            return sb.toString();
        } catch (Throwable e) {
            Log.w(TAG, "自动登录异常，将显示登录页: " + e.getMessage());
            return null;
        }
    }

    /** 把会话 cookie 注入 WebView，使首次加载即为已登录状态。 */
    private void injectSessionCookies(String cookieHeader) {
        try {
            CookieManager cm = CookieManager.getInstance();
            cm.setAcceptCookie(true);
            for (String pair : cookieHeader.split(";")) {
                cm.setCookie(DASHBOARD_URL, pair.trim());
            }
            cm.flush();
        } catch (Throwable e) {
            Log.w(TAG, "注入会话 cookie 失败: " + e.getMessage());
        }
    }

    /** 最小 JSON 字符串转义：密码里可能带引号或反斜杠。 */
    private static String jsonEscape(String s) {
        StringBuilder sb = new StringBuilder(s.length() + 8);
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            switch (c) {
                case '"':
                    sb.append("\\\"");
                    break;
                case '\\':
                    sb.append("\\\\");
                    break;
                case '\n':
                    sb.append("\\n");
                    break;
                case '\r':
                    sb.append("\\r");
                    break;
                default:
                    sb.append(c);
            }
        }
        return sb.toString();
    }

    /** 判断 URL 是否指向本机面板（仅 127.0.0.1 / localhost 的 8088 端口）。 */
    private boolean isLocalPanelUrl(String url) {
        if (url == null) return false;
        return url.startsWith("http://127.0.0.1:8088")
                || url.startsWith("http://localhost:8088")
                || url.equals("about:blank");
    }

}
