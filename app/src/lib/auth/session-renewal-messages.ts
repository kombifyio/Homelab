/**
 * Session renewal wording (workspace platform/standards/AUTHENTICATION.md §6,
 * "Session renewal, never a sign-in error"). A signed-in person is never told
 * they are signed out or should sign in again: when renewal needs a gesture
 * the product asks neutrally for a confirmation, with the action still
 * pending.
 */
export const sessionRenewalMessages = {
  en: {
    "sessionRenewal.title": "Confirm to continue",
    "sessionRenewal.body":
      "kombify needs a quick confirmation before it finishes this. Everything you entered stays as it is.",
    "sessionRenewal.continue": "Continue",
    "sessionRenewal.notNow": "Not now",
    "sessionRenewal.confirming": "Confirming…",
    "sessionRenewal.blocked":
      "Your browser did not open the confirmation window. Allow pop-ups for this site and continue, or continue in this tab and come back to this page afterwards.",
    "sessionRenewal.sameTab": "Continue in this tab",
    "sessionRenewal.pending":
      "This is waiting for your confirmation. Nothing was changed yet.",
    "sessionRenewal.tenantContextMissing":
      "This session has no kombify organization, so its data cannot be shown here. Open Homelab Runtime from your kombify Cloud portal, or continue to renew the session.",
    "sessionRenewal.windowTitle": "One moment",
    "sessionRenewal.windowBody":
      "kombify is confirming your session. This window closes by itself.",
  },
  de: {
    "sessionRenewal.title": "Zum Fortfahren bestätigen",
    "sessionRenewal.body":
      "kombify braucht eine kurze Bestätigung, bevor es das abschließt. Alles, was du eingegeben hast, bleibt erhalten.",
    "sessionRenewal.continue": "Fortfahren",
    "sessionRenewal.notNow": "Nicht jetzt",
    "sessionRenewal.confirming": "Wird bestätigt…",
    "sessionRenewal.blocked":
      "Dein Browser hat das Bestätigungsfenster nicht geöffnet. Erlaube Pop-ups für diese Seite und fahre fort, oder fahre in diesem Tab fort und kehre danach auf diese Seite zurück.",
    "sessionRenewal.sameTab": "In diesem Tab fortfahren",
    "sessionRenewal.pending":
      "Das wartet auf deine Bestätigung. Es wurde noch nichts geändert.",
    "sessionRenewal.tenantContextMissing":
      "Diese Sitzung hat keine kombify-Organisation, deshalb können ihre Daten hier nicht angezeigt werden. Öffne Homelab Runtime über dein kombify-Cloud-Portal oder fahre fort, um die Sitzung zu erneuern.",
    "sessionRenewal.windowTitle": "Einen Moment",
    "sessionRenewal.windowBody":
      "kombify bestätigt deine Sitzung. Dieses Fenster schließt sich von selbst.",
  },
  es: {
    "sessionRenewal.title": "Confirma para continuar",
    "sessionRenewal.body":
      "kombify necesita una confirmación rápida antes de terminar esto. Todo lo que introdujiste se mantiene.",
    "sessionRenewal.continue": "Continuar",
    "sessionRenewal.notNow": "Ahora no",
    "sessionRenewal.confirming": "Confirmando…",
    "sessionRenewal.blocked":
      "Tu navegador no abrió la ventana de confirmación. Permite las ventanas emergentes para este sitio y continúa, o continúa en esta pestaña y vuelve a esta página después.",
    "sessionRenewal.sameTab": "Continuar en esta pestaña",
    "sessionRenewal.pending":
      "Esto espera tu confirmación. Todavía no se ha cambiado nada.",
    "sessionRenewal.tenantContextMissing":
      "Esta sesión no tiene ninguna organización de kombify, así que sus datos no se pueden mostrar aquí. Abre Homelab Runtime desde tu portal de kombify Cloud o continúa para renovar la sesión.",
    "sessionRenewal.windowTitle": "Un momento",
    "sessionRenewal.windowBody":
      "kombify está confirmando tu sesión. Esta ventana se cierra sola.",
  },
  "zh-Hans": {
    "sessionRenewal.title": "确认后继续",
    "sessionRenewal.body":
      "kombify 需要你快速确认一下才能完成此操作。你输入的所有内容都会保留。",
    "sessionRenewal.continue": "继续",
    "sessionRenewal.notNow": "暂不",
    "sessionRenewal.confirming": "正在确认…",
    "sessionRenewal.blocked":
      "你的浏览器没有打开确认窗口。请允许此网站的弹出窗口后继续，或在此标签页中继续，之后会回到此页面。",
    "sessionRenewal.sameTab": "在此标签页中继续",
    "sessionRenewal.pending": "此操作正在等待你的确认，目前尚未做任何更改。",
    "sessionRenewal.tenantContextMissing":
      "此会话没有 kombify 组织，因此无法在此显示其数据。请从 kombify Cloud 门户打开 Homelab Runtime，或继续以更新会话。",
    "sessionRenewal.windowTitle": "请稍候",
    "sessionRenewal.windowBody": "kombify 正在确认你的会话。此窗口会自动关闭。",
  },
  hi: {
    "sessionRenewal.title": "जारी रखने के लिए पुष्टि करें",
    "sessionRenewal.body":
      "इसे पूरा करने से पहले kombify को एक छोटी-सी पुष्टि चाहिए। आपने जो कुछ भी दर्ज किया है, वह वैसा ही रहेगा।",
    "sessionRenewal.continue": "जारी रखें",
    "sessionRenewal.notNow": "अभी नहीं",
    "sessionRenewal.confirming": "पुष्टि हो रही है…",
    "sessionRenewal.blocked":
      "आपके ब्राउज़र ने पुष्टि वाली विंडो नहीं खोली। इस साइट के लिए पॉप-अप की अनुमति दें और जारी रखें, या इसी टैब में जारी रखें और बाद में इस पेज पर लौट आएँ।",
    "sessionRenewal.sameTab": "इसी टैब में जारी रखें",
    "sessionRenewal.pending":
      "यह आपकी पुष्टि की प्रतीक्षा कर रहा है। अभी तक कुछ भी नहीं बदला गया है।",
    "sessionRenewal.tenantContextMissing":
      "इस सत्र में कोई kombify संगठन नहीं है, इसलिए इसका डेटा यहाँ नहीं दिखाया जा सकता। अपने kombify Cloud पोर्टल से Homelab Runtime खोलें, या सत्र को नवीनीकृत करने के लिए जारी रखें।",
    "sessionRenewal.windowTitle": "एक क्षण",
    "sessionRenewal.windowBody":
      "kombify आपके सत्र की पुष्टि कर रहा है। यह विंडो अपने-आप बंद हो जाएगी।",
  },
  ar: {
    "sessionRenewal.title": "أكِّد للمتابعة",
    "sessionRenewal.body":
      "يحتاج kombify إلى تأكيد سريع قبل إنهاء هذا الإجراء. يبقى كل ما أدخلته كما هو.",
    "sessionRenewal.continue": "متابعة",
    "sessionRenewal.notNow": "ليس الآن",
    "sessionRenewal.confirming": "جارٍ التأكيد…",
    "sessionRenewal.blocked":
      "لم يفتح متصفحك نافذة التأكيد. اسمح بالنوافذ المنبثقة لهذا الموقع ثم تابع، أو تابع في علامة التبويب هذه وعُد إلى هذه الصفحة بعد ذلك.",
    "sessionRenewal.sameTab": "المتابعة في علامة التبويب هذه",
    "sessionRenewal.pending": "ينتظر هذا الإجراء تأكيدك. لم يتغير شيء بعد.",
    "sessionRenewal.tenantContextMissing":
      "لا تحتوي هذه الجلسة على مؤسسة kombify، لذا لا يمكن عرض بياناتها هنا. افتح Homelab Runtime من بوابة kombify Cloud، أو تابع لتجديد الجلسة.",
    "sessionRenewal.windowTitle": "لحظة من فضلك",
    "sessionRenewal.windowBody":
      "يؤكد kombify جلستك الآن. ستُغلق هذه النافذة تلقائيًا.",
  },
} as const;
